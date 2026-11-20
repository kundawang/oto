// Copyright 2021 The Oto Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package oto

import (
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/ebitengine/oto/v3/internal/mathutil"
	"github.com/ebitengine/oto/v3/internal/mux"
)

var (
	contextCreated       bool
	contextCreationMutex sync.Mutex
)

// DuckingConfig describes how players in a target category are attenuated
// while at least one player in a trigger category is playing.
//
// While the trigger is active, the target's gain moves from 1 down to Gain over
// Attack, and while no trigger is active, the gain moves back from Gain to 1
// over Release. The gain multiplies each player's volume.
//
// A gain out of the range [0, 1] is clamped to it. A non-finite gain, including
// NaN, is treated as 1, disabling the attenuation. A negative Attack or Release
// is treated as 0, which applies the change at once.
type DuckingConfig struct {
	// Gain is the volume multiplier applied to the target category while the
	// trigger category plays. 0 silences the target, and 1 does not attenuate
	// it.
	Gain float64

	// Attack is the time the target's gain takes to fall from 1 to Gain.
	Attack time.Duration

	// Release is the time the target's gain takes to rise from Gain to 1.
	// Release is also used when a trigger stops while another one ducks the
	// target, and the gain moves up to the remaining trigger's level.
	Release time.Duration
}

// SetDucking configures how players in the trigger category attenuate players
// in the target category while they are playing. A typical use ducks background
// music under a voice prompt.
//
// An empty trigger or target category names no player, and the rule is ignored.
// Setting a rule for the same pair of categories replaces its configuration.
//
// A player is in a category, with a priority, by Player.SetCategory. Only a
// playing player triggers ducking. The gain multiplies the target players'
// volumes, and both channels of a frame get the same gain.
//
// The gain follows the sample time of the mix: the ramps are measured in frames
// of the Context's sample rate, and they advance with the frames mixed, so a
// mix that pauses does not advance them. A player that pauses is no longer a
// trigger, and the target starts releasing. A closed player is likewise no
// longer a trigger.
//
// When several rules duck the same target, the rule whose trigger category has
// the highest priority among its playing players applies; a tie goes to the
// stronger attenuation. The target does not release while any trigger is
// playing: when one stops and another continues, the gain moves to the
// remaining trigger's level at the Release rate.
//
// Ducking has no effect on a Context whose mix is not read: a Context that is
// suspended keeps its gain, and applies it from where it left off on Resume.
//
// SetDucking is concurrent-safe.
func (c *Context) SetDucking(trigger, target Category, config DuckingConfig) {
	c.context.mux.SetDucking(mux.Category(trigger), mux.Category(target), mux.DuckingConfig(config))
}

// ClearDucking removes the rule from the trigger category to the target
// category. A target that is ducked at the time releases over the last
// configured Release.
//
// ClearDucking is concurrent-safe.
func (c *Context) ClearDucking(trigger, target Category) {
	c.context.mux.ClearDucking(mux.Category(trigger), mux.Category(target))
}

// Context is the main object in Oto. It interacts with the audio drivers.
//
// To play sound with Oto, first create a context. Then use the context to create
// an arbitrary number of players. Then use the players to play sound.
//
// Creating multiple contexts is NOT supported.
type Context struct {
	context *context
}

// Format is the format of sources.
type Format int

const (
	// FormatFloat32LE is the format of 32-bit floats in little endian.
	FormatFloat32LE Format = iota

	// FormatUnsignedInt8 is the format of 8-bit integers.
	FormatUnsignedInt8

	// FormatSignedInt16LE is the format of 16-bit integers in little endian.
	FormatSignedInt16LE
)

// NewContextOptions represents options for NewContext.
type NewContextOptions struct {
	// SampleRate specifies the number of samples that should be played during one second.
	// Typical values are 44100 or 48000. One context has only one sample rate. You cannot play multiple audio
	// sources with different sample rates at the same time.
	SampleRate int

	// ChannelCount specifies the number of channels. One channel is mono playback. Two
	// channels are stereo playback. No other values are supported.
	ChannelCount int

	// Format specifies the format of sources.
	Format Format

	// BufferSize specifies a buffer size in the underlying device.
	// BufferSize must not be negative.
	//
	// If 0 is specified, the driver's default buffer size is used.
	// Set BufferSize to adjust the buffer size if you want to adjust latency or reduce noise.
	// A buffer size that is too big increases the latency.
	// On the other hand, a buffer size that is too small can cause glitch noises due to buffer shortage.
	BufferSize time.Duration

	// ApplicationName specifies the name of the client application.
	// It is used for PulseAudio's volume control UI and so on.
	ApplicationName string
}

// NewContext creates a new context with given options.
// The options must not be nil.
// A context creates and holds ready-to-use Player objects.
// NewContext returns a context, a channel that closes when initialization finishes, and an error, if any.
// After the channel closes, call Context.Err to check whether initialization succeeded.
//
// NewContext can be called only once in a process. Any later call returns an error.
func NewContext(options *NewContextOptions) (*Context, chan struct{}, error) {
	if options == nil {
		return nil, nil, fmt.Errorf("oto: options must not be nil")
	}
	if options.SampleRate <= 0 {
		return nil, nil, fmt.Errorf("oto: sample rate must be positive: %d", options.SampleRate)
	}
	if options.ChannelCount != 1 && options.ChannelCount != 2 {
		return nil, nil, fmt.Errorf("oto: channel count must be 1 or 2: %d", options.ChannelCount)
	}
	if options.BufferSize < 0 {
		return nil, nil, fmt.Errorf("oto: buffer size must not be negative: %s", options.BufferSize)
	}

	contextCreationMutex.Lock()
	defer contextCreationMutex.Unlock()

	if contextCreated {
		return nil, nil, fmt.Errorf("oto: context is already created")
	}
	contextCreated = true

	bufferSizeInBytes, err := durationToBufferSize(options.BufferSize, options.SampleRate, options.ChannelCount)
	if err != nil {
		return nil, nil, err
	}
	ctx, ready, err := newContext(options.SampleRate, options.ChannelCount, mux.Format(options.Format), bufferSizeInBytes, options.ApplicationName)
	if err != nil {
		return nil, nil, err
	}
	return &Context{context: ctx}, ready, nil
}

func durationToBufferSize(duration time.Duration, sampleRate, channelCount int) (int, error) {
	if duration == 0 {
		return 0, nil
	}
	if duration < 0 || sampleRate <= 0 || channelCount <= 0 {
		return 0, fmt.Errorf("oto: invalid buffer duration, sample rate, or channel count")
	}

	// The underlying driver always uses 32-bit floats.
	const maxInt = int(^uint(0) >> 1)
	if channelCount > maxInt/4 {
		return 0, fmt.Errorf("oto: buffer frame size exceeds int range")
	}
	bytesPerFrame := channelCount * 4

	frames, ok := mathutil.MulDiv(int64(duration), int64(sampleRate), int64(time.Second))
	if !ok || frames > int64(maxInt/bytesPerFrame) {
		return 0, fmt.Errorf("oto: buffer size exceeds int range")
	}
	return int(frames) * bytesPerFrame, nil
}

// NewPlayer creates a new, ready-to-use Player belonging to the Context.
// It is safe to create multiple players.
//
// The returned player must be kept reachable as long as it should keep playing.
// A player is closed when it becomes unreachable, even in the middle of playing.
//
// The format of r is as follows:
//
//	[data]      = [sample 1] [sample 2] [sample 3] ...
//	[sample *]  = [channel 1] [channel 2] ...
//	[channel *] = [byte 1] [byte 2] ...
//
// Byte ordering is little endian.
//
// A player has some amount of an underlying buffer.
// Read data from r is queued to the player's underlying buffer.
// The underlying buffer is consumed by its playing.
// Then, r's position and the current playing position don't necessarily match.
// If you want to seek the position of r, call the player's Seek function,
// which also clears the underlying buffer.
// If you want to stop using r (e.g. you want to close r), call the player's PauseAndStopReading function.
//
// You cannot share r by multiple players.
// Reads from players' sources are performed sequentially. A Read that blocks
// can prevent other players in the same Context from receiving more data.
//
// The returned player is a *Player, which has functions like SetBufferSize and Seek.
// You can modify the buffer size of a player by the SetBufferSize function.
// A small buffer size is useful if you want to play a real-time PCM for example.
// Note that the audio quality might be affected if you modify the buffer size.
//
// If r does not implement io.Seeker, the returned player's Seek returns an error.
//
// NewPlayer is concurrent-safe.
//
// All the functions of a Player returned by NewPlayer are concurrent-safe.
func (c *Context) NewPlayer(r io.Reader) *Player {
	return &Player{
		player: c.context.mux.NewPlayer(r),
	}
}

// Suspend suspends the entire audio play.
//
// Suspend is concurrent-safe.
func (c *Context) Suspend() error {
	return c.context.Suspend()
}

// Resume resumes the entire audio play, which was suspended by Suspend.
//
// Resume is concurrent-safe.
func (c *Context) Resume() error {
	return c.context.Resume()
}

// Err returns an error that occurred in the audio driver, if any.
// Errors reported by Err are fatal: once Err returns a non-nil error,
// this context is no longer usable.
//
// Err is concurrent-safe.
func (c *Context) Err() error {
	return c.context.Err()
}

type atomicError struct {
	err error
	m   sync.Mutex
}

// Join records err in addition to the errors recorded so far. A nil err is
// ignored.
func (a *atomicError) Join(err error) {
	if err == nil {
		return
	}

	a.m.Lock()
	defer a.m.Unlock()
	a.err = errors.Join(a.err, err)
}

func (a *atomicError) Load() error {
	a.m.Lock()
	defer a.m.Unlock()
	return a.err
}
