package update

import (
	"os"
	"strings"
)

// InDocker reports whether the process runs in a container: the image sets
// SEEDMIRAGE_IN_DOCKER=1; /.dockerenv covers other ways of running it.
func InDocker() bool {
	return inDocker(os.Getenv, func(p string) bool { _, err := os.Stat(p); return err == nil })
}

func inDocker(getenv func(string) string, exists func(string) bool) bool {
	switch strings.ToLower(strings.TrimSpace(getenv("SEEDMIRAGE_IN_DOCKER"))) {
	case "1", "true", "yes", "on":
		return true
	}
	return exists("/.dockerenv")
}
