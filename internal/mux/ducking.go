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

package mux

import (
	"math"
	"time"
)

// Category names a group of players for ducking.
//
// The empty Category is no category: a player in no category neither triggers
// ducking nor is ducked, and a rule with an empty trigger or target is ignored.
type Category string

// DuckingConfig describes how players in a target category are attenuated
// while at least one player in a trigger category is playing.
//
// While the trigger is active, the target's gain moves from 1 down to Gain over
// Attack, and while no trigger is active, the gain moves back from Gain to 1
// over Release. A player's manual volume multiplies the gain.
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

// duckKey identifies a rule by the trigger and the target categories.
type duckKey struct {
	trigger Category
	target  Category
}

// ducking is a normalized DuckingConfig.
type ducking struct {
	// gain is DuckingConfig.Gain, clamped to [0, 1].
	gain float64
	// attackFrames and releaseFrames are the lengths of the ramps in frames.
	// Zero means that the change is applied at once.
	attackFrames  float64
	releaseFrames float64
}

// newDucking normalizes config for a mux of the given sample rate.
func newDucking(config DuckingConfig, sampleRate int) ducking {
	gain := config.Gain
	switch {
	case math.IsNaN(gain) || math.IsInf(gain, 0):
		// A non-finite gain is no attenuation rather than silence. Mixing with
		// a non-finite gain would make every sample of the target non-finite.
		gain = 1
	case gain < 0:
		gain = 0
	case gain > 1:
		gain = 1
	}
	return ducking{
		gain:          gain,
		attackFrames:  durationFrames(config.Attack, sampleRate),
		releaseFrames: durationFrames(config.Release, sampleRate),
	}
}

// durationFrames returns d in frames at sampleRate. A non-positive duration is
// 0 frames, which means an immediate change.
func durationFrames(d time.Duration, sampleRate int) float64 {
	if d <= 0 || sampleRate <= 0 {
		return 0
	}
	return float64(d) * float64(sampleRate) / float64(time.Second)
}

// attackerStep returns how far the gain falls a frame when it moves down to
// target. A zero attack changes at once.
func (d ducking) attackerStep(target float64) float64 {
	if d.attackFrames <= 0 || target >= 1 {
		return math.Inf(1)
	}
	return (1 - target) / d.attackFrames
}

// releaserStep returns how far the gain rises a frame when it moves up to
// target. A zero release changes at once.
//
// The rate is the rule's own depth over Release, so the whole release takes
// Release however far the gain has to rise. A rule whose Gain is 1 has no depth
// of its own to scale by, so it releases the whole range over Release; such a
// rule can only release ducking left by another rule.
func (d ducking) releaserStep(target float64) float64 {
	if d.releaseFrames <= 0 || target <= 0 {
		return math.Inf(1)
	}
	depth := 1 - d.gain
	if depth <= 0 {
		depth = 1
	}
	return depth / d.releaseFrames
}

// duckState is the ducking envelope of one target category.
type duckState struct {
	// gain is the gain of the target category after the last mix.
	gain float64
	// lastConfig is the configuration that last controlled the state, and is
	// used to release the gain after its rule is cleared or its trigger stops.
	lastConfig ducking
	hasConfig  bool
}

// isDuckTriggerLocked reports whether any rule has the category as its trigger.
// The mutex duckMu must be held.
func (m *Mux) isDuckTriggerLocked(category Category) bool {
	for key := range m.duckRules {
		if key.trigger == category {
			return true
		}
	}
	return false
}

// SetDucking configures how players in the trigger category attenuate players
// in the target category while they are playing.
//
// An empty trigger or target category names no player, and the rule is ignored.
// Setting a rule for the same pair of categories replaces its configuration.
//
// When several rules duck the same target, the rule of the trigger category
// with the highest priority among its playing players applies. A tie goes to
// the stronger attenuation. While any trigger remains, the target stays ducked,
// and the gain moves to the remaining trigger's level rather than back to 1.
//
// The gain moves with the sample time of the mix: Attack and Release are
// measured in frames of the Mux's sample rate, and the gain of each frame is
// the same for every channel. A gain is never applied to the sample before the
// one that triggered the change.
//
// SetDucking is concurrent-safe.
func (m *Mux) SetDucking(trigger, target Category, config DuckingConfig) {
	if trigger == "" || target == "" {
		return
	}
	normalized := newDucking(config, m.sampleRate)

	m.duckMu.Lock()
	defer m.duckMu.Unlock()

	if m.duckRules == nil {
		m.duckRules = map[duckKey]ducking{}
	}
	m.duckRules[duckKey{
		trigger: trigger,
		target:  target,
	}] = normalized
	m.duckingEnabled.Store(true)
}

// ClearDucking removes the rule from the trigger category to the target
// category. A target that is ducked at the time releases over the last
// configured Release time.
//
// ClearDucking is concurrent-safe.
func (m *Mux) ClearDucking(trigger, target Category) {
	m.duckMu.Lock()
	defer m.duckMu.Unlock()

	delete(m.duckRules, duckKey{
		trigger: trigger,
		target:  target,
	})
}

// resetDucking drops all ducking rules and state. After Stop, later mixes are
// no longer attenuated.
func (m *Mux) resetDucking() {
	m.duckMu.Lock()
	defer m.duckMu.Unlock()

	m.duckRules = nil
	m.duckStates = nil
	m.duckTargets = nil
	m.duckActive = nil
	m.duckingEnabled.Store(false)
}

// hasDuckingLocked reports whether any target's gain is away from 1. The
// mutex duckMu must be held.
func (m *Mux) hasDuckingLocked() bool {
	for _, state := range m.duckStates {
		if state.gain != 1 {
			return true
		}
	}
	return false
}

// appendDuckTargetLocked appends category to the targets to update unless it
// is already there. The mutex duckMu must be held.
func (m *Mux) appendDuckTargetLocked(category Category) {
	for _, c := range m.duckTargets {
		if c == category {
			return
		}
	}
	m.duckTargets = append(m.duckTargets, category)
}

// selectDuckRuleLocked returns the rule that controls target, if any. Among
// the rules whose trigger is active, the one with the highest trigger priority
// wins; a tie goes to the stronger attenuation. The mutex duckMu must be held.
func (m *Mux) selectDuckRuleLocked(target Category, active map[Category]int) (ducking, bool) {
	var best ducking
	bestPriority := 0
	found := false
	for key, rule := range m.duckRules {
		if key.target != target {
			continue
		}
		priority, ok := active[key.trigger]
		if !ok {
			continue
		}
		if !found || priority > bestPriority || (priority == bestPriority && rule.gain < best.gain) {
			best = rule
			bestPriority = priority
			found = true
		}
	}
	return best, found
}

// updateDuckGainsLocked advances every target's envelope by frames and returns
// the gain of each frame for the targets that are attenuated, keyed by target
// category. A category with no entry has the gain 1 throughout.
//
// The mutex duckMu must be held.
func (m *Mux) updateDuckGainsLocked(frames int, active map[Category]int) map[Category][]float32 {
	if m.duckStates == nil {
		m.duckStates = map[Category]*duckState{}
	}
	if m.duckBuffers == nil {
		m.duckBuffers = map[Category][]float32{}
	}
	if m.duckGains == nil {
		m.duckGains = map[Category][]float32{}
	} else {
		clear(m.duckGains)
	}

	m.duckTargets = m.duckTargets[:0]
	for key := range m.duckRules {
		m.appendDuckTargetLocked(key.target)
	}
	for category, state := range m.duckStates {
		if state.gain != 1 {
			m.appendDuckTargetLocked(category)
		}
	}

	for _, target := range m.duckTargets {
		state := m.duckStates[target]
		if state == nil {
			state = &duckState{gain: 1}
			m.duckStates[target] = state
		}

		rule, triggerActive := m.selectDuckRuleLocked(target, active)
		targetGain := 1.0
		config := state.lastConfig
		switch {
		case triggerActive:
			targetGain = rule.gain
			config = rule
			state.lastConfig = rule
			state.hasConfig = true
		case !state.hasConfig:
			config = ducking{gain: 1}
		}

		if frames <= 0 || (state.gain == 1 && targetGain == 1) {
			continue
		}

		gains := m.duckBuffers[target]
		if cap(gains) < frames {
			gains = make([]float32, frames)
		} else {
			gains = gains[:frames]
		}
		m.duckBuffers[target] = gains

		gain := state.gain
		switch {
		case gain > targetGain:
			step := config.attackerStep(targetGain)
			for i := range frames {
				gain -= step
				if gain < targetGain {
					gain = targetGain
				}
				gains[i] = float32(gain)
			}
		case gain < targetGain:
			step := config.releaserStep(targetGain)
			for i := range frames {
				gain += step
				if gain > targetGain {
					gain = targetGain
				}
				gains[i] = float32(gain)
			}
		default:
			for i := range frames {
				gains[i] = float32(gain)
			}
		}
		state.gain = gain
		m.duckGains[target] = gains
	}

	// Once every rule is cleared and every envelope is back at 1, ducking no
	// longer has to be considered for a mix.
	if len(m.duckRules) == 0 && !m.hasDuckingLocked() {
		m.duckingEnabled.Store(false)
	}
	return m.duckGains
}
