package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"sort"
	"strconv"
	"time"

	"k8s.io/klog"
)

type releaseReport struct {
	healthyMessages   []string
	unhealthyMessages []string
}

type report struct {
	streams       map[string]*releaseReport
	oldestMinor   int
	newestMinor   int
	releaseAPIUrl string
}

func generateReport(acceptedStalenessLimit, builtStalenessLimit, upgradeStalenessLimit time.Duration, oldestMinor, newestMinor int, arch string) (*report, error) {

	releaseAPIUrl, found := releaseAPIUrls[arch]
	if !found {
		return nil, fmt.Errorf("unknown architecture: %s", arch)
	}
	acceptedReleases, err := getReleaseStream(releaseAPIUrl + acceptedReleasePath)
	if err != nil {
		return nil, err
	}
	allReleases, err := getReleaseStream(releaseAPIUrl + allReleasePath)
	if err != nil {
		return nil, err
	}

	// stable graph only includes successful edges.  nightly+prerelease include edges for any upgrade attempt that was
	// made, regardless of whether the job passed.
	stableGraph, err := getUpgradeGraph(releaseAPIUrl, "stable")
	if err != nil {
		return nil, err
	}

	report := checkUpgrades(stableGraph, allReleases, upgradeStalenessLimit, oldestMinor, newestMinor)
	report.releaseAPIUrl = releaseAPIUrl

	klog.V(4).Info("Checking streams for accepted payloads\n")
	acceptedEmpty, acceptedStale := getEmptyAndStaleStreams(acceptedReleases, acceptedStalenessLimit, oldestMinor, newestMinor, releaseAPIUrl)
	klog.V(4).Info("Checking streams for all payloads\n")
	allEmpty, allStale := getEmptyAndStaleStreams(allReleases, acceptedStalenessLimit, oldestMinor, newestMinor, releaseAPIUrl)

	for stream, _ := range acceptedEmpty {
		klog.V(4).Infof("Examining stream %s which has no accepted payloads", stream)
		// if there are no accepted payloads, but the overall payloads set for the stream is not empty
		// (and especially if the overall payloads are not stale), flag it.  If the overall stream is empty,
		// we'll flag it further below.
		if _, ok := allStale[stream]; !ok {
			report.streams[stream].unhealthyMessages = append(report.streams[stream].unhealthyMessages, "Has no accepted payloads, but the stream contains recently built payloads")
		} else if _, ok := allEmpty[stream]; !ok {
			report.streams[stream].unhealthyMessages = append(report.streams[stream].unhealthyMessages, "Has no accepted payloads, but the stream contains built payloads")
		}

	}
	for stream, age := range acceptedStale {
		report.streams[stream].unhealthyMessages = append(report.streams[stream].unhealthyMessages, fmt.Sprintf("Most recently accepted payload > %.1f days, last accepted was %.1f days ago", acceptedStalenessLimit.Hours()/24, age.Hours()/24))
	}

	for stream, _ := range allEmpty {
		report.streams[stream].unhealthyMessages = append(report.streams[stream].unhealthyMessages, "Has no built payloads")
	}

	klog.V(4).Infof("Checking streams for very stale payloads\n")
	_, allVeryStale := getEmptyAndStaleStreams(allReleases, builtStalenessLimit, oldestMinor, newestMinor, releaseAPIUrl)

	for stream, age := range allVeryStale {
		report.streams[stream].unhealthyMessages = append(report.streams[stream].unhealthyMessages, fmt.Sprintf("Most recently built payload was %.1f days ago", age.Hours()/24))
	}

	return report, nil
}

func (rep *report) String(includeHealthy bool) string {
	streams := []string{}
	for stream, _ := range rep.streams {
		streams = append(streams, stream)
	}

	sort.Strings(streams)
	sort.Slice(streams, func(i, j int) bool {
		iMatches := extractMinorRegex.FindStringSubmatch(streams[i])
		iVersion, _ := strconv.Atoi(iMatches[1])
		jMatches := extractMinorRegex.FindStringSubmatch(streams[j])
		jVersion, _ := strconv.Atoi(jMatches[1])
		// this deliberately reverses the standard sorting order so we
		// get highest to lowest.
		return iVersion > jVersion

	})

	output := ""

	for _, stream := range streams {
		if len(rep.streams[stream].unhealthyMessages) == 0 && !includeHealthy {
			continue // nothing to say about this healthy stream
		}

		output += fmt.Sprintf(rep.releaseAPIUrl + "/#" + stream + "\n")

		unhealthyPrefix := ""
		if includeHealthy {
			unhealthyPrefix = "*WARNING:* "
		}
		for _, o := range rep.streams[stream].unhealthyMessages {
			output += fmt.Sprintf("  * %s%s\n", unhealthyPrefix, o)
		}

		if includeHealthy {
			for _, o := range rep.streams[stream].healthyMessages {
				output += fmt.Sprintf("  * %s\n", o)
			}
		}

		output += "\n"
	}
	if !includeHealthy && len(output) == 0 {
		output += "No unhealthy payload streams detected\n"
	}
	output += fmt.Sprintf("\nIgnored releases older than 4.%d.z and newer than 4.%d.z\n", rep.oldestMinor, rep.newestMinor)
	return output
}

func getReleaseStream(url string) (map[string][]string, error) {
	res, err := http.Get(url)
	if err != nil {
		return nil, fmt.Errorf("error fetching releases from %s: %s", url, err)
	}
	if res.StatusCode != 200 {
		return nil, fmt.Errorf("non-OK http response code from %s: %d", url, res.StatusCode)
	}

	releases := make(map[string][]string)

	err = json.NewDecoder(res.Body).Decode(&releases)
	if err != nil {
		return nil, fmt.Errorf("error decoding releases from %s: %v", url, err)
	}

	return releases, nil
}

func getEmptyAndStaleStreams(releases map[string][]string, threshold time.Duration, oldestMinor, newestMinor int, releaseAPIUrl string) (map[string]struct{}, map[string]time.Duration) {
	emptyStreams := make(map[string]struct{})
	staleStreams := make(map[string]time.Duration)
	releaseKeys := reflect.ValueOf(releases).MapKeys()
	now := time.Now()
	for _, k := range releaseKeys {
		stream := k.String()

		matches := zReleaseRegex.FindStringSubmatch(stream)

		if matches == nil {
			//fmt.Printf("ignoring non z-stream release %s\n", stream)
			continue
		}
		if v, _ := strconv.Atoi(matches[1]); v < oldestMinor {
			klog.V(4).Infof("ignoring release %s because it is older than the oldest desired minor %d\n", stream, oldestMinor)
			continue
		}
		if v, _ := strconv.Atoi(matches[1]); v > newestMinor {
			klog.V(4).Infof("ignoring release %s because it is newer than the newest desired minor %d\n", stream, newestMinor)
			continue
		}
		if len(releases[stream]) == 0 {
			klog.V(4).Infof("Release %s has no payloads\n", stream)
			emptyStreams[stream] = struct{}{}
			continue
		}
		freshPayload := false
		var newest time.Time
		for _, payload := range releases[stream] {
			ts, err := getPayloadTimestamp(payload)
			if err != nil {
				klog.Errorf(err.Error())
				continue
			}
			delta := now.Sub(ts)
			if delta.Minutes() < threshold.Minutes() {
				klog.V(4).Infof("Release %s in stream %s is fresh: %0.1f hours old (threshold is %0.1f)\n", payload, stream, delta.Hours(), threshold.Hours())
				freshPayload = true
			} else {
				klog.V(4).Infof("Release %s in stream %s is stale: %0.1f hours old (threshold is %0.1f)\n", payload, stream, delta.Hours(), threshold.Hours())
			}
			if ts.After(newest) {
				newest = ts
			}
		}
		if !freshPayload {
			klog.V(4).Infof("Release stream %s does not have a recent payload: "+releaseAPIUrl+"/#"+stream+"\n", stream)
			staleStreams[stream] = now.Sub(newest)
		}
	}
	return emptyStreams, staleStreams
}

func getPayloadTimestamp(payload string) (time.Time, error) {
	m := extractDateRegex.FindStringSubmatch(payload)
	if m == nil || len(m) != 7 {
		return time.Time{}, fmt.Errorf("error: could not extract date from payload %s", payload)
	}
	//fmt.Printf("Release %s has date %s\n", r, m[0])
	//t := time.Date(m[1], m[2], m[3], m[4], m[5], m[6], 0, time.UTC)
	payloadTime, err := time.Parse("2006-01-02-150405 MST", m[0]+" EST")
	if err != nil {
		return time.Time{}, fmt.Errorf("error: failed to parse time string %s: %v", m[0], err)
	}
	//fmt.Printf("%v\n", t)
	return payloadTime, nil

}

type GraphNode struct {
	Version string `json:"version"`
	Payload string `json:"payload"`
	From    int
}

type GraphEdge [2]int

type Graph struct {
	Nodes []GraphNode `json:"nodes"`
	Edges []GraphEdge `json:"edges"`
}

type GraphMap map[string][]string

func getUpgradeGraph(apiurl, channel string) (GraphMap, error) {
	graphMap := GraphMap{}

	graph := Graph{}
	url := apiurl + "/graph?channel=" + channel
	res, err := http.Get(url)
	if err != nil {
		return graphMap, fmt.Errorf("error fetching upgrade graph from %s: %s", url, err)
	}
	if res.StatusCode != 200 {
		return graphMap, fmt.Errorf("non-OK http response code fetching upgrade graph from %s: %d", url, res.StatusCode)
	}

	err = json.NewDecoder(res.Body).Decode(&graph)
	if err != nil {
		return graphMap, fmt.Errorf("error decoding upgrade graph: %v", err)
	}

	for _, edge := range graph.Edges {
		from := edge[0]
		to := edge[1]
		graph.Nodes[to].From = from
		if _, ok := graphMap[graph.Nodes[to].Version]; !ok {
			graphMap[graph.Nodes[to].Version] = []string{graph.Nodes[from].Version}
		} else {
			graphMap[graph.Nodes[to].Version] = append(graphMap[graph.Nodes[to].Version], graph.Nodes[from].Version)
		}
	}

	return graphMap, nil
}

type found struct {
	Version string
	Age     time.Duration
}

func (f *found) Days() float64 {
	return f.Age.Hours() / 24
}

func checkUpgrades(graph GraphMap, releases map[string][]string, stalenessThreshold time.Duration, oldestMinor, newestMinor int) *report {
	rep := &report{
		streams:     make(map[string]*releaseReport, len(releases)),
		oldestMinor: oldestMinor,
		newestMinor: newestMinor,
	}

	now := time.Now()
	for release, payloads := range releases {

		matches := zReleaseRegex.FindStringSubmatch(release)

		if matches == nil {
			klog.V(4).Infof("not checking upgrade status for non z-stream release %s", release)
			continue
		}
		v, _ := strconv.Atoi(matches[1])
		if v < oldestMinor {
			klog.V(4).Infof("ignoring release %s because it is older than the oldest desired minor %d\n", release, oldestMinor)
			continue
		}
		if v > newestMinor {
			klog.V(4).Infof("ignoring release %s because it is newer than the newest desired minor %d\n", release, newestMinor)
			continue
		}

		var foundMinor *found
		var foundPatch *found
		rep.streams[release] = &releaseReport{}
		for _, payload := range payloads {
			ts, err := getPayloadTimestamp(payload)
			if err != nil {
				klog.Error(err.Error())
				continue
			}
			age := now.Sub(ts)
			if age.Minutes() > stalenessThreshold.Minutes() {
				continue
			}
			toMatches := extractMinorRegex.FindStringSubmatch(payload)
			if toMatches == nil {
				continue
			}
			toVersion, _ := strconv.Atoi(toMatches[1])

			for _, from := range graph[payload] {

				fromMatches := extractMinorRegex.FindStringSubmatch(from)

				if fromMatches == nil {
					klog.V(4).Infof("Ignoring upgrade to %s from %s because the minor version could not be determined\n", payload, from)
					continue
				}
				fromVersion, _ := strconv.Atoi(fromMatches[1])

				klog.V(4).Infof("Payload %s successfully upgrades from %s\n", payload, from)
				if toVersion == fromVersion {
					foundPatch = &found{
						Version: from,
						Age:     age,
					}
				}
				if toVersion == fromVersion+1 {
					foundMinor = &found{
						Version: from,
						Age:     age,
					}
				}
				if foundMinor != nil && foundPatch != nil {
					// we have found a recent payload in the set of payloads this release, which successfully upgraded from a previous minor
					// and a previous patch, so we don't need to continue checking payloads for this release.
					break
				}
			}
		}

		if foundPatch == nil {
			rep.streams[release].unhealthyMessages = append(rep.streams[release].unhealthyMessages, "Does not have a recent valid patch level upgrade")
		} else {
			rep.streams[release].healthyMessages = append(rep.streams[release].healthyMessages, fmt.Sprintf("Has a recent valid patch level upgrade from %s %0.1f days ago", foundPatch.Version, foundPatch.Days()))
		}
		if foundMinor == nil {
			rep.streams[release].unhealthyMessages = append(rep.streams[release].unhealthyMessages, "Does not have a recent valid minor level upgrade")
		} else {
			rep.streams[release].healthyMessages = append(rep.streams[release].healthyMessages, fmt.Sprintf("Has a recent valid minor level upgrade from %s %0.1f days ago", foundMinor.Version, foundMinor.Days()))
		}
	}
	return rep
}
