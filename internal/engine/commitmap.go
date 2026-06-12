package engine

import (
	"bufio"
	"fmt"
	"os"
	"strings"
)

// ErrUnmappedCommit is returned when a filtered SHA has no entry in the
// commit-map. Per spec §10 we fail with guidance and never guess.
type ErrUnmappedCommit struct{ FilteredSHA string }

func (e *ErrUnmappedCommit) Error() string {
	return fmt.Sprintf("filtered commit %s not found in commit-map — run a sync first; "+
		"if the agent branched from history older than this bridge, the base cannot be resolved", e.FilteredSHA)
}

// LookupRealSHA resolves filtered-sha → real-sha from the commit-map file
// written by the last sync (lines: "<real-sha> <filtered-sha>").
func LookupRealSHA(mapPath, filteredSHA string) (string, error) {
	f, err := os.Open(mapPath)
	if err != nil {
		if os.IsNotExist(err) {
			return "", fmt.Errorf("commit-map missing at %s — run a sync first", mapPath)
		}
		return "", err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) == 2 && fields[1] == filteredSHA {
			return fields[0], nil
		}
	}
	if err := sc.Err(); err != nil {
		return "", err
	}
	return "", &ErrUnmappedCommit{FilteredSHA: filteredSHA}
}
