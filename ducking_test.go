// Copyright 2026 The Oto Authors
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

package oto_test

import (
	"bytes"
	"math"
	"testing"
	"time"

	"github.com/ebitengine/oto/v3"
)

// The tests here use theContext from oto_test.go's TestMain, the one context a
// process can create. They check the API's behavior through the real audio
// drivers; the envelope's sample-level behavior is checked in internal/mux.

// repeatPCM returns frames frames of stereo float32 samples of value.
func repeatPCM(frames int, value float32) []byte {
	bs := make([]byte, frames*8)
	bits := math.Float32bits(value)
	for i := range frames {
		for c := range 2 {
			o := (i*2 + c) * 4
			bs[o] = byte(bits)
			bs[o+1] = byte(bits >> 8)
			bs[o+2] = byte(bits >> 16)
			bs[o+3] = byte(bits >> 24)
		}
	}
	return bs
}

func TestCategory(t *testing.T) {
	const (
		music = oto.Category("music")
		voice = oto.Category("voice")
	)

	p := theContext.NewPlayer(bytes.NewReader(repeatPCM(4800, 0.25)))
	t.Cleanup(func() { p.PauseAndStopReading() })

	if got := p.Category(); got != "" {
		t.Errorf("default Category: got %q; want the empty category", got)
	}
	p.SetCategory(music, 1)
	if got := p.Category(); got != music {
		t.Errorf("Category after SetCategory: got %q; want %q", got, music)
	}
	p.SetCategory("", 0)
	if got := p.Category(); got != "" {
		t.Errorf("Category after resetting: got %q; want the empty category", got)
	}

	// A rule with an empty category is ignored rather than failing.
	theContext.SetDucking("", music, oto.DuckingConfig{Gain: 0.5})
	theContext.SetDucking(voice, "", oto.DuckingConfig{Gain: 0.5})
	theContext.SetDucking(voice, music, oto.DuckingConfig{Gain: 0.5})
	theContext.ClearDucking(voice, music)
	theContext.ClearDucking("", music)

	if err := theContext.Err(); err != nil {
		t.Errorf("Err after ducking configuration: got %v; want nil", err)
	}
}

// TestDuckingWhilePlaying checks that a trigger playing and pausing with the
// real driver leaves the target usable: both players keep reporting being
// played, and no error follows.
func TestDuckingWhilePlaying(t *testing.T) {
	if testing.Short() {
		t.Skip("plays sound for a while")
	}

	const (
		music = oto.Category("music")
		voice = oto.Category("voice")
	)

	target := theContext.NewPlayer(bytes.NewReader(repeatPCM(48000, 0.25)))
	t.Cleanup(func() { target.PauseAndStopReading() })
	target.SetCategory(music, 0)
	target.Play()

	trigger := theContext.NewPlayer(bytes.NewReader(repeatPCM(48000, 0.25)))
	t.Cleanup(func() { trigger.PauseAndStopReading() })
	trigger.SetCategory(voice, 0)

	theContext.SetDucking(voice, music, oto.DuckingConfig{
		Gain:    0.25,
		Attack:  10 * time.Millisecond,
		Release: 20 * time.Millisecond,
	})
	t.Cleanup(func() { theContext.ClearDucking(voice, music) })

	for range 4 {
		trigger.Play()
		time.Sleep(30 * time.Millisecond)
		trigger.Pause()
		time.Sleep(30 * time.Millisecond)
	}

	if !target.IsPlaying() {
		t.Error("the target stopped playing while it was ducked")
	}
	if err := target.Err(); err != nil {
		t.Errorf("target Err: got %v; want nil", err)
	}
	if err := trigger.Err(); err != nil {
		t.Errorf("trigger Err: got %v; want nil", err)
	}
	if err := theContext.Err(); err != nil {
		t.Errorf("Context Err: got %v; want nil", err)
	}
}
