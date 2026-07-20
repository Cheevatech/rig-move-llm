//go:build !darwin && !linux

package tui

import (
	"errors"
	"os"
)

// On platforms where we do not implement termios raw mode (e.g. Windows), Select
// and Confirm degrade to a numbered line prompt — still colored, still describing
// every option, just without arrow/space navigation.
const rawSupported = false

type termState struct{}

func makeRaw(*os.File) (*termState, error) { return nil, errors.New("raw mode unsupported") }

func (s *termState) restore() {}
