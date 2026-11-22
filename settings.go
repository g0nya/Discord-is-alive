package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
)

type gameFilterMode uint8

const (
	gameFilterOff gameFilterMode = iota
	gameFilterAll
	gameFilterSiege
)

func (mode gameFilterMode) label() string {
	switch mode {
	case gameFilterAll:
		return "все игры"
	case gameFilterSiege:
		return "Rainbow Six Siege"
	default:
		return "выкл."
	}
}

type settings struct {
	GameFilter  bool `json:"game_filter"`
	SiegeFilter bool `json:"siege_filter,omitempty"`
}

func readGameFilter(dir string) (gameFilterMode, error) {
	data, err := os.ReadFile(filepath.Join(dir, "settings.json"))
	if errors.Is(err, os.ErrNotExist) {
		return gameFilterOff, nil
	}
	if err != nil {
		return gameFilterOff, err
	}
	var s settings
	if err := json.Unmarshal(data, &s); err != nil {
		return gameFilterOff, err
	}
	if s.SiegeFilter {
		return gameFilterSiege, nil
	}
	if s.GameFilter {
		return gameFilterAll, nil
	}
	return gameFilterOff, nil
}

func writeGameFilter(dir string, mode gameFilterMode) error {
	data, err := json.MarshalIndent(settings{
		GameFilter:  mode == gameFilterAll,
		SiegeFilter: mode == gameFilterSiege,
	}, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomic(filepath.Join(dir, "settings.json"), append(data, '\n'))
}
