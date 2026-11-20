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

package mux_test

import (
	"bytes"
	"math"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ebitengine/oto/v3/internal/mux"
)

const duckCategory = mux.Category("duck-category")

// duckSample is the value that a target player produces at the volume 1.
const duckSample = 0.5

// newSamplePlayer creates a stereo player that produces sample on both channels.
func newSamplePlayer(t *testing.T, m *mux.Mux, sample float32) *mux.Player {
	t.Helper()

	p := newPlayer(t, m, &float32Reader{value: sample})
	p.Play()
	return p
}

// mixFrames mixes frames frames and returns the left and right values of each.
func mixFrames(t *testing.T, m *mux.Mux, frames int) (left, right []float32) {
	t.Helper()

	buf := make([]float32, frames*2)
	m.ReadFloat32s(buf)
	left = make([]float32, frames)
	right = make([]float32, frames)
	for i := range frames {
		left[i] = buf[2*i]
		right[i] = buf[2*i+1]
	}
	return left, right
}

// wantGain returns the sample that duckSample takes at gain.
func wantGain(gain float64) float32 {
	return float32(float64(duckSample) * gain)
}

// checkGain compares got with the sample that duckSample takes at gain.
func checkGain(t *testing.T, frame int, got float32, gain float64) {
	t.Helper()

	want := wantGain(gain)
	if diff := math.Abs(float64(got - want)); diff > 1e-4 {
		t.Errorf("frame %d: got %v; want %v", frame, got, want)
	}
}

// waitForDuckBuffered waits until a player of a float32 source has buffered
// enough to mix frames frames. The source reads in real time here, so this
// waits with a deadline rather than in virtual time.
func waitForDuckBuffered(t *testing.T, p *mux.Player, frames int) {
	t.Helper()
	waitForBufferedSize(t, p, frames*8)
}

func TestDuckingAttenuatesTargetWhileTriggerPlays(t *testing.T) {
	const frames = 16

	m := mux.New(48000, 2, mux.FormatFloat32LE)
	defer m.Stop()

	target := newSamplePlayer(t, m, duckSample)
	target.SetCategory(duckCategory, 0)
	trigger := newSamplePlayer(t, m, 0)
	trigger.SetCategory("voice", 0)
	m.SetDucking("voice", duckCategory, mux.DuckingConfig{
		Gain:    0.25,
		Attack:  0,
		Release: 0,
	})

	waitForDuckBuffered(t, target, frames)
	waitForDuckBuffered(t, trigger, frames)

	left, right := mixFrames(t, m, frames)
	for i := range frames {
		checkGain(t, i, left[i], 0.25)
		checkGain(t, i, right[i], 0.25)
	}
}

func TestDuckingDisabledKeepsTheMix(t *testing.T) {
	const frames = 16

	m := mux.New(48000, 2, mux.FormatFloat32LE)
	defer m.Stop()

	target := newSamplePlayer(t, m, duckSample)
	target.SetCategory(duckCategory, 0)
	// The trigger is never in a category, so no rule applies.
	trigger := newSamplePlayer(t, m, 0)
	m.SetDucking("voice", duckCategory, mux.DuckingConfig{Gain: 0})

	waitForDuckBuffered(t, target, frames)
	waitForDuckBuffered(t, trigger, frames)

	left, right := mixFrames(t, m, frames)
	for i := range frames {
		if got := left[i]; got != duckSample {
			t.Errorf("frame %d left: got %v; want %v", i, got, duckSample)
		}
		if got := right[i]; got != duckSample {
			t.Errorf("frame %d right: got %v; want %v", i, got, duckSample)
		}
	}
}

func TestDuckingAttackAndReleaseFollowSampleTime(t *testing.T) {
	const frames = 16

	// The gain ramps over exactly frames frames in each direction, so the
	// expected values are easy to write down.
	m := mux.New(48000, 2, mux.FormatFloat32LE)
	defer m.Stop()

	target := newSamplePlayer(t, m, duckSample)
	target.SetCategory(duckCategory, 0)
	trigger := newSamplePlayer(t, m, 0)
	trigger.SetCategory("voice", 0)
	attack := frameTime(frames)
	m.SetDucking("voice", duckCategory, mux.DuckingConfig{
		Gain:    0,
		Attack:  attack,
		Release: attack,
	})

	waitForDuckBuffered(t, target, 4*frames)
	waitForDuckBuffered(t, trigger, 4*frames)

	// The attack starts with the first frame after the trigger was seen
	// playing, and the gain of the first frame is one step down from 1.
	left, _ := mixFrames(t, m, frames)
	for i := range frames {
		// The gain after i+1 steps of 1/frames.
		checkGain(t, i, left[i], 1-float64(i+1)/frames)
	}

	// The final gain of the attack is 0, and it stays there while the trigger
	// plays.
	left, _ = mixFrames(t, m, 4)
	for i := range left {
		if left[i] != 0 {
			t.Errorf("held frame %d: got %v; want 0", i, left[i])
		}
	}

	// The trigger stops, and the gain releases over the same time.
	trigger.Pause()
	left, _ = mixFrames(t, m, frames)
	for i := range frames {
		checkGain(t, i, left[i], float64(i+1)/frames)
	}

	// The gain ends at 1 and stays there.
	left, _ = mixFrames(t, m, 4)
	for i := range left {
		if left[i] != duckSample {
			t.Errorf("released frame %d: got %v; want %v", i, left[i], duckSample)
		}
	}
}

func TestDuckingOverlapDoesNotReleaseEarly(t *testing.T) {
	const frames = 16

	m := mux.New(48000, 2, mux.FormatFloat32LE)
	defer m.Stop()

	target := newSamplePlayer(t, m, duckSample)
	target.SetCategory(duckCategory, 0)
	first := newSamplePlayer(t, m, 0)
	first.SetCategory("voice", 0)
	second := newSamplePlayer(t, m, 0)
	second.SetCategory("voice", 0)
	m.SetDucking("voice", duckCategory, mux.DuckingConfig{Gain: 0, Attack: frameTime(1), Release: frameTime(frames)})

	waitForDuckBuffered(t, target, 4*frames)
	waitForDuckBuffered(t, first, 4*frames)
	waitForDuckBuffered(t, second, 4*frames)

	// The gain reaches 0 at once.
	mixFrames(t, m, 1)

	// The first trigger stops while the second keeps playing. The target must
	// stay ducked although one of the two triggers ended.
	first.Pause()
	left, _ := mixFrames(t, m, frames)
	for i := range left {
		if left[i] != 0 {
			t.Errorf("frame %d with one trigger left: got %v; want 0", i, left[i])
		}
	}

	// When the last trigger stops, the gain releases.
	second.Pause()
	left, _ = mixFrames(t, m, frames)
	for i := range left {
		checkGain(t, i, left[i], float64(i+1)/frames)
	}
}

func TestDuckingFollowsTheStrongestPriority(t *testing.T) {
	const frames = 8

	m := mux.New(48000, 2, mux.FormatFloat32LE)
	defer m.Stop()

	target := newSamplePlayer(t, m, duckSample)
	target.SetCategory(duckCategory, 0)
	quiet := newSamplePlayer(t, m, 0)
	quiet.SetCategory("quiet", 0)
	strong := newSamplePlayer(t, m, 0)
	strong.SetCategory("strong", 10)
	m.SetDucking("quiet", duckCategory, mux.DuckingConfig{Gain: 0.8})
	m.SetDucking("strong", duckCategory, mux.DuckingConfig{Gain: 0.25})

	waitForDuckBuffered(t, target, frames)
	waitForDuckBuffered(t, quiet, frames)
	waitForDuckBuffered(t, strong, frames)

	left, _ := mixFrames(t, m, frames)
	for i := range left {
		if want := float32(duckSample * 0.25); left[i] != want {
			t.Errorf("frame %d: got %v; want the strong trigger's %v", i, left[i], want)
		}
	}
}

// A higher priority trigger that pauses hands the target to the remaining
// trigger's attenuation: the target does not return to 1 while a trigger plays.
func TestDuckingHandsOverToTheRemainingTrigger(t *testing.T) {
	const frames = 8

	m := mux.New(48000, 2, mux.FormatFloat32LE)
	defer m.Stop()

	target := newSamplePlayer(t, m, duckSample)
	target.SetCategory(duckCategory, 0)
	quiet := newSamplePlayer(t, m, 0)
	quiet.SetCategory("quiet", 0)
	strong := newSamplePlayer(t, m, 0)
	strong.SetCategory("strong", 10)
	m.SetDucking("quiet", duckCategory, mux.DuckingConfig{Gain: 0.5, Attack: frameTime(1), Release: frameTime(frames)})
	m.SetDucking("strong", duckCategory, mux.DuckingConfig{Gain: 0.25, Attack: frameTime(1), Release: frameTime(frames)})

	waitForDuckBuffered(t, target, 3*frames)
	waitForDuckBuffered(t, quiet, 3*frames)
	waitForDuckBuffered(t, strong, 3*frames)

	// The strong trigger sets the gain to 0.25.
	mixFrames(t, m, 1)

	strong.Pause()
	left, _ := mixFrames(t, m, frames)
	// The gain rises to 0.5 at the remaining rule's rate, not to 1.
	for i := range left {
		gain := min(0.5, 0.25+0.5*float64(i+1)/frames)
		checkGain(t, i, left[i], gain)
	}
	left, _ = mixFrames(t, m, frames)
	for i := range left {
		checkGain(t, i, left[i], 0.5)
	}
}

func TestDuckingTakesPriorityOverAReplacedRule(t *testing.T) {
	const frames = 8

	m := mux.New(48000, 2, mux.FormatFloat32LE)
	defer m.Stop()

	target := newSamplePlayer(t, m, duckSample)
	target.SetCategory(duckCategory, 0)
	trigger := newSamplePlayer(t, m, 0)
	trigger.SetCategory("voice", 0)
	m.SetDucking("voice", duckCategory, mux.DuckingConfig{Gain: 0, Attack: 0, Release: 0})

	waitForDuckBuffered(t, target, frames)
	waitForDuckBuffered(t, trigger, frames)
	mixFrames(t, m, 1)

	// Replacing the rule applies the new gain, without a release in between.
	m.SetDucking("voice", duckCategory, mux.DuckingConfig{Gain: 0.5, Attack: 0, Release: 0})
	left, _ := mixFrames(t, m, frames)
	for i := range left {
		checkGain(t, i, left[i], 0.5)
	}
}

func TestDuckingClearedRuleReleases(t *testing.T) {
	const frames = 8

	m := mux.New(48000, 2, mux.FormatFloat32LE)
	defer m.Stop()

	target := newSamplePlayer(t, m, duckSample)
	target.SetCategory(duckCategory, 0)
	trigger := newSamplePlayer(t, m, 0)
	trigger.SetCategory("voice", 0)
	m.SetDucking("voice", duckCategory, mux.DuckingConfig{Gain: 0, Attack: 0, Release: frameTime(frames)})

	waitForDuckBuffered(t, target, 2*frames)
	waitForDuckBuffered(t, trigger, 2*frames)
	mixFrames(t, m, 1)

	// The trigger keeps playing, but the rule is gone.
	m.ClearDucking("voice", duckCategory)
	left, _ := mixFrames(t, m, frames)
	for i := range left {
		checkGain(t, i, left[i], float64(i+1)/frames)
	}
}

func TestDuckingEmptyCategoriesAreIgnored(t *testing.T) {
	const frames = 8

	m := mux.New(48000, 2, mux.FormatFloat32LE)
	defer m.Stop()

	target := newSamplePlayer(t, m, duckSample)
	trigger := newSamplePlayer(t, m, 0)
	for _, tt := range []struct{ trigger, target mux.Category }{
		{"", duckCategory},
		{"voice", ""},
		{"", ""},
	} {
		m.SetDucking(tt.trigger, tt.target, mux.DuckingConfig{Gain: 0})
	}

	waitForDuckBuffered(t, target, frames)
	waitForDuckBuffered(t, trigger, frames)
	left, _ := mixFrames(t, m, frames)
	for i := range left {
		if left[i] != duckSample {
			t.Errorf("frame %d: got %v; want %v", i, left[i], duckSample)
		}
	}
}

func TestDuckingGainIsAppliedWithTheVolume(t *testing.T) {
	const frames = 8

	m := mux.New(48000, 2, mux.FormatFloat32LE)
	defer m.Stop()

	target := newPlayer(t, m, &float32Reader{value: duckSample})
	target.SetVolume(0.5)
	target.Play()
	target.SetCategory(duckCategory, 0)
	trigger := newSamplePlayer(t, m, 0)
	trigger.SetCategory("voice", 0)
	m.SetDucking("voice", duckCategory, mux.DuckingConfig{Gain: 0.5})

	waitForDuckBuffered(t, target, frames)
	waitForDuckBuffered(t, trigger, frames)
	left, right := mixFrames(t, m, frames)
	for i := range frames {
		checkGain(t, i, left[i], 0.5*0.5)
		checkGain(t, i, right[i], 0.5*0.5)
	}
}

func TestDuckingInvalidConfigKeepsTheMixFinite(t *testing.T) {
	const frames = 8

	for _, tt := range []struct {
		name   string
		config mux.DuckingConfig
	}{
		{
			name:   "NaN gain",
			config: mux.DuckingConfig{Gain: math.NaN()},
		},
		{
			name:   "positive infinity gain",
			config: mux.DuckingConfig{Gain: math.Inf(1)},
		},
		{
			name:   "negative infinity gain",
			config: mux.DuckingConfig{Gain: math.Inf(-1)},
		},
		{
			name:   "negative gain",
			config: mux.DuckingConfig{Gain: -1},
		},
		{
			name:   "gain above one",
			config: mux.DuckingConfig{Gain: 2},
		},
		{
			name:   "negative durations",
			config: mux.DuckingConfig{Gain: 0, Attack: -time.Second, Release: -time.Second},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			m := mux.New(48000, 2, mux.FormatFloat32LE)
			defer m.Stop()

			target := newSamplePlayer(t, m, duckSample)
			target.SetCategory(duckCategory, 0)
			trigger := newSamplePlayer(t, m, 0)
			trigger.SetCategory("voice", 0)
			m.SetDucking("voice", duckCategory, tt.config)

			waitForDuckBuffered(t, target, frames)
			waitForDuckBuffered(t, trigger, frames)
			left, _ := mixFrames(t, m, frames)
			var want float64
			switch {
			case math.IsNaN(tt.config.Gain), math.IsInf(tt.config.Gain, 0):
				want = 1
			case tt.config.Gain < 0:
				want = 0
			case tt.config.Gain > 1:
				want = 1
			default:
				want = tt.config.Gain
			}
			for i := range left {
				if math.IsNaN(float64(left[i])) || math.IsInf(float64(left[i]), 0) {
					t.Fatalf("frame %d: got %v; want a finite value", i, left[i])
				}
				checkGain(t, i, left[i], want)
			}
		})
	}
}

func TestDuckingPausedPlayerDoesNotTrigger(t *testing.T) {
	const frames = 8

	m := mux.New(48000, 2, mux.FormatFloat32LE)
	defer m.Stop()

	target := newSamplePlayer(t, m, duckSample)
	target.SetCategory(duckCategory, 0)
	trigger := newSamplePlayer(t, m, 0)
	trigger.SetCategory("voice", 0)
	m.SetDucking("voice", duckCategory, mux.DuckingConfig{Gain: 0, Attack: 0, Release: frameTime(frames)})

	waitForDuckBuffered(t, target, 2*frames)
	waitForDuckBuffered(t, trigger, 2*frames)
	mixFrames(t, m, 1)

	// A paused trigger releases the target.
	trigger.Pause()
	left, _ := mixFrames(t, m, frames)
	for i := range left {
		checkGain(t, i, left[i], float64(i+1)/frames)
	}
}

func TestDuckingClosedPlayerDoesNotTrigger(t *testing.T) {
	const frames = 8

	m := mux.New(48000, 2, mux.FormatFloat32LE)
	defer m.Stop()

	target := newSamplePlayer(t, m, duckSample)
	target.SetCategory(duckCategory, 0)
	trigger := newPlayer(t, m, &emptyReader{})
	trigger.SetCategory("voice", 0)
	trigger.Play()
	m.SetDucking("voice", duckCategory, mux.DuckingConfig{Gain: 0, Attack: 0, Release: frameTime(frames)})

	waitForDuckBuffered(t, target, 2*frames)
	mixFrames(t, m, 1)

	_ = trigger.Close()
	left, _ := mixFrames(t, m, frames)
	for i := range left {
		checkGain(t, i, left[i], float64(i+1)/frames)
	}
}

func TestDuckingZeroFramesMixesNothing(t *testing.T) {
	m := mux.New(48000, 2, mux.FormatFloat32LE)
	defer m.Stop()

	target := newSamplePlayer(t, m, duckSample)
	target.SetCategory(duckCategory, 0)
	trigger := newSamplePlayer(t, m, 0)
	trigger.SetCategory("voice", 0)
	m.SetDucking("voice", duckCategory, mux.DuckingConfig{Gain: 0, Attack: frameTime(4), Release: frameTime(4)})

	waitForDuckBuffered(t, target, 8)
	waitForDuckBuffered(t, trigger, 8)

	// A mix with no frames must not advance the envelope, and must not panic.
	mixFrames(t, m, 0)
	left, _ := mixFrames(t, m, 4)
	for i := range left {
		checkGain(t, i, left[i], 1-float64(i+1)/4)
	}
}

func TestDuckingStopsWithTheMux(t *testing.T) {
	const frames = 8

	m := mux.New(48000, 2, mux.FormatFloat32LE)
	target := newSamplePlayer(t, m, duckSample)
	target.SetCategory(duckCategory, 0)
	trigger := newSamplePlayer(t, m, 0)
	trigger.SetCategory("voice", 0)
	m.SetDucking("voice", duckCategory, mux.DuckingConfig{Gain: 0, Attack: 0, Release: 0})

	waitForDuckBuffered(t, target, frames)
	waitForDuckBuffered(t, trigger, frames)
	mixFrames(t, m, 1)

	m.Stop()
	// Once the mux is stopped, its rules are dropped and the mix is no longer
	// attenuated.
	left, _ := mixFrames(t, m, frames)
	for i := range left {
		if left[i] != duckSample {
			t.Errorf("frame %d after Stop: got %v; want %v", i, left[i], duckSample)
		}
	}
}

func TestDuckingCanBeSetAfterTheMixStarted(t *testing.T) {
	const frames = 8

	m := mux.New(48000, 2, mux.FormatFloat32LE)
	defer m.Stop()

	target := newSamplePlayer(t, m, duckSample)
	target.SetCategory(duckCategory, 0)
	trigger := newSamplePlayer(t, m, 0)
	trigger.SetCategory("voice", 0)

	waitForDuckBuffered(t, target, 2*frames)
	waitForDuckBuffered(t, trigger, 2*frames)

	// No rule yet.
	left, _ := mixFrames(t, m, frames)
	for i := range left {
		if left[i] != duckSample {
			t.Fatalf("frame %d before the rule: got %v; want %v", i, left[i], duckSample)
		}
	}

	m.SetDucking("voice", duckCategory, mux.DuckingConfig{Gain: 0, Attack: 0, Release: 0})
	left, _ = mixFrames(t, m, frames)
	for i := range left {
		if left[i] != 0 {
			t.Errorf("frame %d after the rule: got %v; want 0", i, left[i])
		}
	}
}

// TestDuckingCategoryChangeTakesEffect checks that a player moved into a target
// category while playing is ducked at once.
func TestDuckingCategoryChangeTakesEffect(t *testing.T) {
	const frames = 8

	m := mux.New(48000, 2, mux.FormatFloat32LE)
	defer m.Stop()

	target := newSamplePlayer(t, m, duckSample)
	trigger := newSamplePlayer(t, m, 0)
	trigger.SetCategory("voice", 0)
	m.SetDucking("voice", duckCategory, mux.DuckingConfig{Gain: 0, Attack: 0, Release: 0})

	waitForDuckBuffered(t, target, 2*frames)
	waitForDuckBuffered(t, trigger, 2*frames)

	// The target is in no category yet.
	left, _ := mixFrames(t, m, frames)
	for i := range left {
		if left[i] != duckSample {
			t.Fatalf("frame %d before the category was set: got %v; want %v", i, left[i], duckSample)
		}
	}

	target.SetCategory(duckCategory, 0)
	left, _ = mixFrames(t, m, frames)
	for i := range left {
		if left[i] != 0 {
			t.Errorf("frame %d after the category was set: got %v; want 0", i, left[i])
		}
	}
}

func TestDuckingCategoryAccessors(t *testing.T) {
	m := mux.New(48000, 2, mux.FormatFloat32LE)
	defer m.Stop()

	p := newPlayer(t, m, &emptyReader{})
	if got := p.Category(); got != "" {
		t.Errorf("default Category: got %q; want the empty category", got)
	}
	p.SetCategory(duckCategory, 3)
	if got := p.Category(); got != duckCategory {
		t.Errorf("Category after SetCategory: got %q; want %q", got, duckCategory)
	}
}

// TestDuckingConcurrentUpdatesRace exercises the locks of ducking: the rules and
// the players' categories change while the mix runs.
func TestDuckingConcurrentUpdatesRace(t *testing.T) {
	m := mux.New(48000, 2, mux.FormatFloat32LE)
	defer m.Stop()

	target := newSamplePlayer(t, m, duckSample)
	trigger := newSamplePlayer(t, m, 0)

	var wg sync.WaitGroup
	var stop atomic.Bool
	wg.Go(func() {
		buf := make([]float32, 64)
		for !stop.Load() {
			m.ReadFloat32s(buf)
			for _, v := range buf {
				if math.IsNaN(float64(v)) || math.IsInf(float64(v), 0) {
					t.Error("a mix with concurrent ducking changes was not finite")
					return
				}
			}
		}
	})
	var inner sync.WaitGroup
	for range 4 {
		inner.Go(func() {
			for i := range 1000 {
				m.SetDucking("voice", duckCategory, mux.DuckingConfig{
					Gain:    0.5,
					Attack:  time.Duration(i) * time.Microsecond,
					Release: time.Duration(i) * time.Microsecond,
				})
				trigger.SetCategory("voice", i%3)
				target.SetCategory(duckCategory, i%2)
				if i%7 == 0 {
					m.ClearDucking("voice", duckCategory)
				}
			}
		})
	}
	inner.Wait()
	stop.Store(true)
	wg.Wait()
}

// TestDuckingRapidTriggerChanges checks that the envelope stays within its
// bounds and finite while a trigger starts and stops every mix.
func TestDuckingRapidTriggerChanges(t *testing.T) {
	const frames = 4

	m := mux.New(48000, 2, mux.FormatFloat32LE)
	defer m.Stop()

	target := newSamplePlayer(t, m, duckSample)
	target.SetCategory(duckCategory, 0)
	trigger := newSamplePlayer(t, m, 0)
	trigger.SetCategory("voice", 0)
	m.SetDucking("voice", duckCategory, mux.DuckingConfig{Gain: 0, Attack: frameTime(frames), Release: frameTime(frames)})

	waitForDuckBuffered(t, target, 512)
	waitForDuckBuffered(t, trigger, 512)

	previous := float32(duckSample)
	for round := range 32 {
		if round%2 == 0 {
			trigger.Play()
		} else {
			trigger.Pause()
		}
		left, right := mixFrames(t, m, frames)
		for i := range frames {
			if math.IsNaN(float64(left[i])) || math.IsInf(float64(left[i]), 0) {
				t.Fatalf("round %d frame %d: got %v; want a finite value", round, i, left[i])
			}
			if left[i] < 0 || left[i] > duckSample {
				t.Fatalf("round %d frame %d: got %v; want a value in [0, %v]", round, i, left[i], duckSample)
			}
			if left[i] != right[i] {
				t.Fatalf("round %d frame %d: left %v and right %v differ", round, i, left[i], right[i])
			}
		}
		// The gain moves by at most the full range in one mix.
		step := float64(left[frames-1] - previous)
		if math.Abs(step) > float64(duckSample)+1e-4 {
			t.Fatalf("round %d: the gain moved by %v in one mix", round, step)
		}
		previous = left[frames-1]
	}
}

// TestDuckingZeroLengthTriggerDoesNotDuck checks that a trigger with no data to
// play does not attenuate the target: an empty source ends at once and is no
// longer playing.
func TestDuckingZeroLengthTriggerDoesNotDuck(t *testing.T) {
	const frames = 8

	m := mux.New(48000, 2, mux.FormatFloat32LE)
	defer m.Stop()

	target := newSamplePlayer(t, m, duckSample)
	target.SetCategory(duckCategory, 0)
	trigger := newPlayer(t, m, bytes.NewReader(nil))
	trigger.SetCategory("voice", 0)
	trigger.Play()
	m.SetDucking("voice", duckCategory, mux.DuckingConfig{Gain: 0, Attack: 0, Release: 0})

	waitForDuckBuffered(t, target, frames)
	// The mux loop finds the end of the empty source.
	synctestWaitForDuck(t, trigger)

	left, _ := mixFrames(t, m, frames)
	for i := range left {
		checkGain(t, i, left[i], 1)
	}
}

// synctestWaitForDuck waits until the mux loop has made its pass over the
// players.
func synctestWaitForDuck(t *testing.T, p *mux.Player) {
	t.Helper()

	deadline := time.Now().Add(time.Second)
	for p.IsPlaying() {
		if time.Now().After(deadline) {
			t.Fatal("the empty trigger stayed playing")
		}
		time.Sleep(time.Millisecond)
	}
}
