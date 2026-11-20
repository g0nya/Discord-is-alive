package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
)

type settings struct {
	GameFilter bool `json:"game_filter"`
}

func readGameFilter(dir string) (bool, error) {
	data, err := os.ReadFile(filepath.Join(dir, "settings.json"))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var s settings
	err = json.Unmarshal(data, &s)
	return s.GameFilter, err
}

func writeGameFilter(dir string, enabled bool) error {
	data, err := json.MarshalIndent(settings{GameFilter: enabled}, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomic(filepath.Join(dir, "settings.json"), append(data, '\n'))
}
