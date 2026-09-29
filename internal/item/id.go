package item

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

var (
	sourceNamePattern  = regexp.MustCompile(`^[a-z0-9-]+$`)
	fingerprintPattern = regexp.MustCompile(`^[A-Za-z0-9]{1,128}$`)
	idPattern          = regexp.MustCompile(`^alert-([a-z0-9-]+)-([A-Za-z0-9]{1,128})-([1-9][0-9]{0,17})$`)
)

// ValidKey reports whether both parts of the key are safe to use in item IDs.
func ValidKey(k Key) bool {
	return sourceNamePattern.MatchString(k.Source) && fingerprintPattern.MatchString(k.Fingerprint)
}

// NewID returns the item ID alert-<source>-<fingerprint>-<n>.
func NewID(k Key, n int) (string, error) {
	if !ValidKey(k) {
		return "", errors.New("invalid source name or fingerprint")
	}
	if n < 1 {
		return "", errors.New("item number must be at least 1")
	}
	return fmt.Sprintf("alert-%s-%s-%d", k.Source, k.Fingerprint, n), nil
}

// ParseID parses an item ID. Fingerprints never contain "-", so the last two
// dash-separated fields are the fingerprint and the counter.
func ParseID(id string) (Key, int, error) {
	m := idPattern.FindStringSubmatch(id)
	if m == nil {
		return Key{}, 0, errors.New("invalid item id")
	}
	n, err := strconv.Atoi(m[3])
	if err != nil {
		return Key{}, 0, errors.New("invalid item id")
	}
	return Key{Source: m[1], Fingerprint: m[2]}, n, nil
}

// String returns "source/fingerprint".
func (k Key) String() string {
	return k.Source + "/" + k.Fingerprint
}

// resourceLabels is the precedence for the resource part of a title.
var resourceLabels = []string{"pod", "deployment", "service", "job"}

// Title derives the item title from alert labels: the alertname, followed by
// ": " and "[namespace/]resource" for the first present resource label.
func Title(labels map[string]string) string {
	title := labels["alertname"]
	for _, l := range resourceLabels {
		res, ok := labels[l]
		if !ok || res == "" {
			continue
		}
		if ns := labels["namespace"]; ns != "" {
			res = ns + "/" + res
		}
		return strings.TrimSpace(title + ": " + res)
	}
	return title
}
