package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"
)

const runLabel = "beesuh.live.run"

type dockerRunner func(...string) ([]byte, error)

var dockerID = regexp.MustCompile(`^[a-f0-9]{64}$`)

func createdID(output []byte) (string, error) {
	for _, line := range strings.Fields(string(output)) {
		if dockerID.MatchString(line) {
			return line, nil
		}
	}
	return "", errors.New("Docker create did not return an actual resource ID")
}
func probeTLS(ctx context.Context, endpoint string) error {
	u, e := url.Parse(endpoint)
	if e != nil || u.Scheme != "https" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return errors.New("invalid fixture HTTPS endpoint")
	}
	// Default Go TLS uses SSL_CERT_FILE and verifies both the CA and hostname.
	client := &http.Client{Timeout: 7 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	req, e := http.NewRequestWithContext(ctx, "GET", endpoint, nil)
	if e != nil {
		return errors.New("invalid fixture request")
	}
	resp, e := client.Do(req)
	if e != nil {
		return errors.New("fixture HTTPS connection or certificate validation failed")
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	if resp.StatusCode != 200 {
		return fmt.Errorf("fixture health status=%d", resp.StatusCode)
	}
	return nil
}
func absent(out []byte, e error) bool {
	text := strings.ToLower(string(out))
	return e != nil && (strings.Contains(text, "no such") || strings.Contains(text, "not found"))
}

// Discover independently by run label and exact run-name prefix, never name inspection alone.
func ownedContainers(run dockerRunner, prefix string) (map[string]bool, error) {
	ids := map[string]bool{}
	if prefix == "" {
		return ids, nil
	}
	for _, filter := range []string{"label=" + runLabel + "=" + prefix, "name=" + prefix} {
		out, e := run("ps", "-a", "--no-trunc", "--filter", filter, "--format", "{{.ID}} {{.Names}}")
		if e != nil {
			return nil, fmt.Errorf("container inventory unavailable for run %s", prefix)
		}
		for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
			fields := strings.Fields(line)
			if len(fields) == 0 {
				continue
			}
			if len(fields) != 2 || !dockerID.MatchString(fields[0]) {
				return nil, errors.New("malformed Docker container inventory")
			}
			if strings.HasPrefix(filter, "label=") || strings.HasPrefix(fields[1], prefix+"-") {
				ids[fields[0]] = true
			}
		}
	}
	return ids, nil
}
func ownedNetworks(run dockerRunner, prefix string) (map[string]bool, error) {
	ids := map[string]bool{}
	if prefix == "" {
		return ids, nil
	}
	for _, filter := range []string{"label=" + runLabel + "=" + prefix, "name=" + prefix} {
		out, e := run("network", "ls", "--no-trunc", "--filter", filter, "--format", "{{.ID}} {{.Name}}")
		if e != nil {
			return nil, fmt.Errorf("network inventory unavailable for run %s", prefix)
		}
		for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
			fields := strings.Fields(line)
			if len(fields) == 0 {
				continue
			}
			if len(fields) != 2 || !dockerID.MatchString(fields[0]) {
				return nil, errors.New("malformed Docker network inventory")
			}
			if strings.HasPrefix(filter, "label=") || fields[1] == prefix {
				ids[fields[0]] = true
			}
		}
	}
	return ids, nil
}
func cleanupDocker(run dockerRunner, prefix string, known []string, networkID string) error {
	ids := map[string]bool{}
	for _, id := range known {
		if id != "" {
			ids[id] = true
		}
	}
	var problems []string
	// Retry discovery/deletion by actual ID to tolerate stale Dory name metadata.
	for attempt := 0; attempt < 3; attempt++ {
		discovered, e := ownedContainers(run, prefix)
		if e != nil {
			problems = append(problems, e.Error())
			break
		}
		for id := range discovered {
			ids[id] = true
		}
		for id := range ids {
			out, e := run("inspect", "--format", "{{.State.Running}}", id)
			if absent(out, e) && !discovered[id] {
				continue
			}
			if e == nil && strings.TrimSpace(string(out)) == "true" {
				_, _ = run("stop", "-t", "3", id)
			}
			_, _ = run("rm", id)
		}
		left, e := ownedContainers(run, prefix)
		allAbsent := true
		for id := range ids {
			out, e := run("inspect", id)
			if !absent(out, e) {
				allAbsent = false
			}
		}
		if e == nil && len(left) == 0 && allAbsent {
			break
		}
	}
	discovered, e := ownedContainers(run, prefix)
	if e != nil {
		problems = append(problems, e.Error())
	} else {
		for id := range discovered {
			ids[id] = true
			problems = append(problems, "container remains "+id)
		}
	}
	for id := range ids {
		out, e := run("inspect", id)
		if !absent(out, e) {
			problems = append(problems, "container removal unverified "+id)
		}
	}
	networks, e := ownedNetworks(run, prefix)
	if e != nil {
		problems = append(problems, e.Error())
		networks = map[string]bool{}
	}
	if networkID != "" {
		networks[networkID] = true
	}
	for attempt := 0; attempt < 3; attempt++ {
		for id := range networks {
			_, _ = run("network", "rm", id)
		}
		left, e := ownedNetworks(run, prefix)
		if e != nil {
			problems = append(problems, e.Error())
			break
		}
		for id := range left {
			networks[id] = true
		}
		allAbsent := true
		for id := range networks {
			out, e := run("network", "inspect", id)
			if !absent(out, e) {
				allAbsent = false
			}
		}
		if len(left) == 0 && allAbsent {
			break
		}
	}
	left, e := ownedNetworks(run, prefix)
	if e != nil {
		problems = append(problems, e.Error())
	} else {
		for id := range left {
			problems = append(problems, "network remains "+id)
		}
	}
	for id := range networks {
		out, e := run("network", "inspect", id)
		if !absent(out, e) {
			problems = append(problems, "network removal unverified "+id)
		}
	}
	if len(problems) > 0 {
		return fmt.Errorf("cleanup unverified for run %s (auth/Bee IDs %s): %s", prefix, strings.Join(known, ","), strings.Join(problems, "; "))
	}
	return nil
}
func (f *fixture) cleanup() error {
	// Every Docker operation uses its own bounded context, even after test cancellation.
	dockerErr := cleanupDocker(boundedDocker, f.network, []string{f.authID, f.beeID}, f.networkID)
	var errorDir error
	if f.dir != "" {
		errorDir = os.RemoveAll(f.dir)
		if errorDir == nil {
			if _, e := os.Stat(f.dir); !os.IsNotExist(e) {
				errorDir = errors.New("fixture directory removal unverified")
			}
		}
	}
	if e := errors.Join(dockerErr, errorDir); e != nil {
		return e
	}
	fmt.Println("PASS owned container IDs, run inventories, network and temp cleanup verified")
	return nil
}
