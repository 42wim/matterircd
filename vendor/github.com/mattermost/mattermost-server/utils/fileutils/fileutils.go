// Copyright (c) 2015-present Mattermost, Inc. All Rights Reserved.
// See License.txt for license information.

package fileutils

import (
	"os"
	"path/filepath"
)

var (
	commonBaseSearchPaths = []string{
		".",
		"..",
		"../..",
		"../../..",
	}
)

func FindPath(path string, baseSearchPaths []string, filter func(os.FileInfo) bool) string {
	if filepath.IsAbs(path) {
		if _, err := os.Stat(path); err == nil {
			return path
		}

		return ""
	}

	searchPaths := []string{}
	searchPaths = append(searchPaths, baseSearchPaths...)

	// Additionally attempt to search relative to the location of the running binary.
	var binaryDir string
	if exe, err := os.Executable(); err == nil {
		if exe, err = filepath.EvalSymlinks(exe); err == nil {
			if exe, err = filepath.Abs(exe); err == nil {
				binaryDir = filepath.Dir(exe)
			}
		}
	}
	if binaryDir != "" {
		for _, baseSearchPath := range baseSearchPaths {
			searchPaths = append(
				searchPaths,
				filepath.Join(binaryDir, baseSearchPath),
			)
		}
	}

	for _, parent := range searchPaths {
		found, err := filepath.Abs(filepath.Join(parent, path))
		if err != nil {
			continue
		} else if fileInfo, err := os.Stat(found); err == nil {
			if filter != nil {
				if filter(fileInfo) {
					return found
				}
			} else {
				return found
			}
		}
	}

	return ""
}

// FindFile looks for the given file in nearby ancestors relative to the current working
// directory as well as the directory of the executable.
func FindFile(path string) string {
	return FindPath(path, commonBaseSearchPaths, func(fileInfo os.FileInfo) bool {
		return !fileInfo.IsDir()
	})
}

// fileutils.FindDir looks for the given directory in nearby ancestors relative to the current working
// directory as well as the directory of the executable, falling back to `./` if not found.
func FindDir(dir string) (string, bool) {
	found := FindPath(dir, commonBaseSearchPaths, func(fileInfo os.FileInfo) bool {
		return fileInfo.IsDir()
	})
	if found == "" {
		return "./", false
	}

	return found, true
}

// FindConfigFile attempts to find an existing configuration file. fileName can be an absolute or
// relative path or name such as "/opt/mattermost/config.json" or simply "config.json". An empty
// string is returned if no configuration is found.
func FindConfigFile(fileName string) (path string) {
	found := FindFile(filepath.Join("config", fileName))
	if found == "" {
		found = FindPath(fileName, []string{"."}, nil)
	}

	return found
}
