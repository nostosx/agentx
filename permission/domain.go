package permission

import (
	"errors"
	"net/url"
	"slices"
	"strings"

	"github.com/nostosx/agentx/internal/fsmatch"
)

// domainPattern is the host pattern of a WebFetch(domain:...) rule; see
// https://code.claude.com/docs/en/permissions#webfetch. Matching is
// case-insensitive and ignores a trailing dot. "*" matches every host, a
// leading "*." matches subdomains at any depth but not the domain itself,
// and any other "*" matches within one label.
type domainPattern struct {
	labels []string
	// sub is set for a leading "*.".
	sub bool
	all bool
}

func parseDomain(s string) (*domainPattern, error) {
	s = normalizeHost(s)
	switch {
	case s == "":
		return nil, errors.New("empty domain")
	case s == "*":
		return &domainPattern{all: true}, nil
	case strings.ContainsAny(s, "/:@?#"):
		return nil, errors.New("a domain rule names a host, not a URL")
	}
	d := &domainPattern{}
	if rest, ok := strings.CutPrefix(s, "*."); ok {
		d.sub, s = true, rest
	}
	d.labels = strings.Split(s, ".")
	if slices.Contains(d.labels, "") {
		return nil, errors.New("empty domain label")
	}
	return d, nil
}

func normalizeHost(s string) string {
	return strings.TrimSuffix(strings.ToLower(strings.TrimSpace(s)), ".")
}

// match reports whether host matches.
func (d *domainPattern) match(host string) bool {
	host = normalizeHost(host)
	if host == "" {
		return false
	}
	if d.all {
		return true
	}
	labels := strings.Split(host, ".")
	if d.sub {
		if len(labels) <= len(d.labels) {
			return false
		}
		labels = labels[len(labels)-len(d.labels):]
	}
	if len(labels) != len(d.labels) {
		return false
	}
	for i, l := range d.labels {
		if !fsmatch.Wildcard(l, labels[i]) {
			return false
		}
	}
	return true
}

// urlHost returns the host of a URL, or "" if it has none.
func urlHost(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return ""
	}
	return u.Hostname()
}
