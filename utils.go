package main

import (
	"fmt"
	"os"
	"strings"
)

// logging

func logRaw(format string, args ...interface{}) {
	fmt.Printf(format+"\n", args...)
}

func logInfo(format string, args ...interface{}) {
	if StartParams.Raw {
		logRaw(format, args...)
	} else {
		logger.Printf(format, args...)
	}
}

func logWarn(format string, args ...interface{}) {
	format = "[WARN] " + format
	if StartParams.Raw {
		logRaw(format, args...)
	} else {
		logger.Printf(format, args...)
	}
}

func logError(format string, args ...interface{}) {
	format = "[ERROR] " + format
	if StartParams.Raw {
		logRaw(format, args...)
	} else {
		logger.Printf(format, args...)
	}
}

func logFatal(format string, args ...interface{}) {
	format = "[FATAL] " + format
	if StartParams.Raw {
		logRaw(format, args...)
	} else {
		logger.Printf(format, args...)
	}
	os.Exit(1)
}

func logFatalError(err error) {
	if err != nil {
		logFatal(err.Error())
	}
}

// strings

type caseSensitivity int

const (
	caseSensitive   caseSensitivity = 0
	caseInsensitive caseSensitivity = 1
)

func startsWith(str, prefix string, cs caseSensitivity) bool {
	if cs == caseSensitive {
		return strings.HasPrefix(str, prefix)
	}
	return strings.HasPrefix(strings.ToLower(str), strings.ToLower(prefix))
}

func startsWithAny(str string, prefixes []string, cs caseSensitivity) bool {
	for _, prefix := range prefixes {
		if startsWith(str, prefix, cs) {
			return true
		}
	}
	return false
}

// file

// Returns if file/dir in path exists.
func fileExists(path string) bool {
	if len(path) == 0 {
		return false
	}
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return false
	}
	return true
}

// data

func stringProperty(data map[string]interface{}, key string) (string, error) {
	if value, ok := data[key]; ok {
		if vStr, ok := value.(string); ok {
			return vStr, nil
		} else {
			return "", fmt.Errorf("%s is not a string", key)
		}
	}
	return "", nil
}
