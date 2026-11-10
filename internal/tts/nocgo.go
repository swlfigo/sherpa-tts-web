//go:build !cgo

package tts

import "errors"

// Load fails: the speech engine is a C library, which needs cgo.
func Load(dir string, threads int) (Synthesizer, error) {
	return nil, errors.New("built without cgo: no speech engine (build with CGO_ENABLED=1)")
}
