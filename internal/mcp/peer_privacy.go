package mcp

import (
	"errors"
	"net/url"
	"sort"
	"strconv"
	"strings"
)

// peerEndpointLabel is a display projection, never a transport URL. Paths,
// userinfo, query values and fragments can all contain configuration secrets.
func peerEndpointLabel(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "(configured endpoint)"
	}
	return (&url.URL{Scheme: u.Scheme, Host: u.Host}).String()
}

// peerFailureText strips configured endpoints from locally generated failures.
// A wrapped URL error can carry an expanded request URL with extra path data.
func peerFailureText(err error, endpoints ...string) string {
	if err == nil {
		return ""
	}
	message := err.Error()
	endpoints = append([]string(nil), endpoints...)
	var requestError *url.Error
	if errors.As(err, &requestError) {
		endpoints = append(endpoints, requestError.URL)
	}
	sort.SliceStable(endpoints, func(i, j int) bool { return len(endpoints[i]) > len(endpoints[j]) })
	for _, raw := range endpoints {
		if raw == "" {
			continue
		}
		label := peerEndpointLabel(raw)
		message = strings.ReplaceAll(message, raw, label)
		quoted := strconv.Quote(raw)
		message = strings.ReplaceAll(message, quoted[1:len(quoted)-1], label)
	}
	return message
}
