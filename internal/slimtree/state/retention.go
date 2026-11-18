package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// retentionFile holds the home's retention rule.
const retentionFile = "retention.json"

// Retention is how many days rm and prune keep an archive and its recovery
// ref. Forever keeps them all.
type Retention int

const (
	Forever Retention = 0
	Week    Retention = 7
	Month   Retention = 30
)

// maxRetentionDays bounds a rule in days. A longer rule is a typo.
const maxRetentionDays = 36500

// RetentionUsage names the rules ParseRetention takes.
const RetentionUsage = "week|month|forever|<N>d"

// ParseRetention parses week, month, forever, or a number of days such as
// 14d.
func ParseRetention(s string) (Retention, error) {
	switch s {
	case "week":
		return Week, nil
	case "month":
		return Month, nil
	case "forever":
		return Forever, nil
	}
	if days, ok := strings.CutSuffix(s, "d"); ok {
		if n, err := strconv.Atoi(days); err == nil && 1 <= n && n <= maxRetentionDays {
			return Retention(n), nil
		}
	}
	return 0, fmt.Errorf("expected week, month, forever, or a number of days from 1d to %dd, not %q", maxRetentionDays, s)
}

func (r Retention) String() string {
	switch r {
	case Forever:
		return "forever"
	case Week:
		return "week"
	case Month:
		return "month"
	}
	return strconv.Itoa(int(r)) + "d"
}

// Retention returns the home's rule: Month when retention.json is missing.
// It returns an error when the file holds no valid rule, because a broken
// rule is no rule to delete by.
func (h *Home) Retention() (Retention, error) {
	path := filepath.Join(h.dir, retentionFile)
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return Month, nil
	}
	var rule map[string]*int
	if err == nil {
		err = json.Unmarshal(data, &rule)
	}
	if err == nil {
		days, ok := rule["max_age_days"]
		switch {
		case !ok:
			err = errors.New("no max_age_days")
		case days == nil:
			return Forever, nil
		case *days < 1 || *days > maxRetentionDays:
			err = fmt.Errorf("max_age_days %d is not from 1 to %d", *days, maxRetentionDays)
		default:
			return Retention(*days), nil
		}
	}
	return 0, fmt.Errorf("%s holds no valid retention rule: %w", path, err)
}

// SetRetention writes r to retention.json atomically.
func (h *Home) SetRetention(r Retention) error {
	var days *int
	if r != Forever {
		n := int(r)
		days = &n
	}
	data, err := json.Marshal(map[string]*int{"max_age_days": days})
	if err != nil {
		return err
	}
	return writeFile(h.dir, retentionFile, append(data, '\n'))
}
