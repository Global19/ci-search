package main

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"html/template"
	"io"
	"io/ioutil"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	units "github.com/docker/go-units"
	"github.com/golang/glog"
)

type nopFlusher struct{}

func (_ nopFlusher) Flush() {}

type Match struct {
	FileType  string   `json:"filename"`
	Context   []string `json:"context,omitempty"`
	MoreLines int      `json:"moreLines,omitempty"`
}

func (o *options) handleConfig(w http.ResponseWriter, req *http.Request) {
	o.ConfigPath = "README.md"
	if o.ConfigPath == "" {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	data, err := ioutil.ReadFile(o.ConfigPath)
	if err != nil {
		http.Error(w, fmt.Sprintf("Unable to read config: %v", err), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	writer := encodedWriter(w, req)
	defer writer.Close()
	if _, err = writer.Write(data); err != nil {
		glog.Errorf("Failed to write response: %v", err)
	}
}

func (o *options) handleIndex(w http.ResponseWriter, req *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		flusher = nopFlusher{}
	}

	index, err := o.parseRequest(req, "text")
	if err != nil {
		http.Error(w, fmt.Sprintf("Bad input: %v", err), http.StatusBadRequest)
		return
	}

	if len(index.Search) == 0 {
		index.Search = []string{""}
	}

	contextOptions := []string{
		fmt.Sprintf(`<option value="-1" %s>Links</option>`, intSelected(1, index.Context)),
		fmt.Sprintf(`<option value="0" %s>No context</option>`, intSelected(0, index.Context)),
		fmt.Sprintf(`<option value="1" %s>1 lines</option>`, intSelected(1, index.Context)),
		fmt.Sprintf(`<option value="2" %s>2 lines</option>`, intSelected(2, index.Context)),
		fmt.Sprintf(`<option value="3" %s>3 lines</option>`, intSelected(3, index.Context)),
		fmt.Sprintf(`<option value="5" %s>5 lines</option>`, intSelected(5, index.Context)),
		fmt.Sprintf(`<option value="7" %s>7 lines</option>`, intSelected(7, index.Context)),
		fmt.Sprintf(`<option value="10" %s>10 lines</option>`, intSelected(10, index.Context)),
		fmt.Sprintf(`<option value="15" %s>15 lines</option>`, intSelected(15, index.Context)),
	}
	switch index.Context {
	case -1, 0, 1, 2, 3, 5, 7, 10, 15:
	default:
		context := template.HTMLEscapeString(strconv.Itoa(index.Context))
		contextOptions = append(contextOptions, fmt.Sprintf(`<option value="%s" selected>%s</option>`, context, context))
	}

	var searchTypeOptions []string
	for _, searchType := range []string{"junit", "build-log", "all"} {
		var selected string
		if searchType == index.SearchType {
			selected = "selected"
		}
		searchTypeOptions = append(searchTypeOptions, fmt.Sprintf(`<option value="%s" %s>%s</option>`, template.HTMLEscapeString(searchType), selected, template.HTMLEscapeString(searchType)))
	}

	maxAgeOptions := []string{
		fmt.Sprintf(`<option value="6h" %s>6h</option>`, durationSelected(6*time.Hour, index.MaxAge)),
		fmt.Sprintf(`<option value="12h" %s>12h</option>`, durationSelected(12*time.Hour, index.MaxAge)),
		fmt.Sprintf(`<option value="24h" %s>1d</option>`, durationSelected(24*time.Hour, index.MaxAge)),
		fmt.Sprintf(`<option value="48h" %s>2d</option>`, durationSelected(48*time.Hour, index.MaxAge)),
		fmt.Sprintf(`<option value="168h" %s>7d</option>`, durationSelected(168*time.Hour, index.MaxAge)),
		fmt.Sprintf(`<option value="336h" %s>14d</option>`, durationSelected(336*time.Hour, index.MaxAge)),
	}
	switch index.MaxAge {
	case 6 * time.Hour, 12 * time.Hour, 24 * time.Hour, 48 * time.Hour, 168 * time.Hour, 336 * time.Hour:
	case 0:
		maxAgeOptions = append(maxAgeOptions, `<option value="0" selected>No limit</option>`)
	default:
		maxAge := template.HTMLEscapeString(index.MaxAge.String())
		maxAgeOptions = append(maxAgeOptions, fmt.Sprintf(`<option value="%s" selected>%s</option>`, maxAge, maxAge))
	}

	writer := encodedWriter(w, req)
	defer writer.Close()
	w.Header().Set("Content-Type", "text/html; charset=utf-8")

	fmt.Fprintf(writer, htmlPageStart, "Search OpenShift CI")
	fmt.Fprintf(writer, htmlIndexForm, template.HTMLEscapeString(index.Search[0]), strings.Join(maxAgeOptions, ""), strings.Join(contextOptions, ""), strings.Join(searchTypeOptions, ""))

	// display the empty results page
	if len(index.Search[0]) == 0 {
		stats := o.accessor.Stats()
		fmt.Fprintf(writer, htmlEmptyPage, units.HumanSize(float64(stats.Size)), stats.Entries)
		fmt.Fprintf(writer, htmlPageEnd)
		return
	}

	// perform a search
	flusher.Flush()
	fmt.Fprintf(writer, `<div style="margin-top: 3rem; position: relative" class="pl-3">`)

	start := time.Now()

	var count int
	if index.Context >= 0 {
		count, err = renderWithContext(req.Context(), writer, index, o.generator, start, o.metadata)
	} else {
		count, err = renderSummary(req.Context(), writer, index, o.generator, start, o.metadata)
	}

	duration := time.Now().Sub(start)
	if err != nil {
		glog.Errorf("Search %q failed with %d results in %s: command failed: %v", index.Search[0], count, duration, err)
		fmt.Fprintf(writer, `<p class="alert alert-danger>%s</p>"`, template.HTMLEscapeString(err.Error()))
		fmt.Fprintf(writer, htmlPageEnd)
		return
	}
	glog.V(2).Infof("Search %q completed with %d results in %s", index.Search[0], count, duration)

	stats := o.accessor.Stats()
	fmt.Fprintf(writer, `<p style="position:absolute; top: -2rem;" class="small"><em>Found %d results in %s (%s in %d entries)</em></p>`, count, duration.Truncate(time.Millisecond), units.HumanSize(float64(stats.Size)), stats.Entries)
	fmt.Fprintf(writer, "</div>")

	fmt.Fprintf(writer, htmlPageEnd)
}

func (o *options) parseRequest(req *http.Request, mode string) (*Index, error) {
	if err := req.ParseForm(); err != nil {
		return nil, err
	}

	index := &Index{}

	index.Search, _ = req.Form["search"]
	if len(index.Search) == 0 && mode == "chart" {
		// Basic source issues
		//index.Search = append(index.Search, "CONFLICT .*Merge conflict in .*")

		// CI-cluster issues
		index.Search = append(index.Search, "could not create or restart template instance.*");
		index.Search = append(index.Search, "could not (wait for|get) build.*");  // https://bugzilla.redhat.com/show_bug.cgi?id=1696483
		/*
		index.Search = append(index.Search, "could not copy .* imagestream.*");  // https://bugzilla.redhat.com/show_bug.cgi?id=1703510
		index.Search = append(index.Search, "error: image .*registry.svc.ci.openshift.org/.* does not exist");
		index.Search = append(index.Search, "unable to find the .* image in the provided release image");
		index.Search = append(index.Search, "error: Process interrupted with signal interrupt.*");
		index.Search = append(index.Search, "pods .* already exists|pod .* was already deleted");
		index.Search = append(index.Search, "could not wait for RPM repo server to deploy.*");
		index.Search = append(index.Search, "could not start the process: fork/exec hack/tests/e2e-scaleupdown-previous.sh: no such file or directory");  // https://openshift-gce-devel.appspot.com/build/origin-ci-test/logs/periodic-ci-azure-e2e-scaleupdown-v4.2/5
		*/

		// Installer and bootstrapping issues issues
		index.Search = append(index.Search, "level=error.*timeout while waiting for state.*");  // https://bugzilla.redhat.com/show_bug.cgi?id=1690069 https://bugzilla.redhat.com/show_bug.cgi?id=1691516
		/*
		index.Search = append(index.Search, "checking install permissions: error simulating policy: Throttling: Rate exceeded");  // https://bugzilla.redhat.com/show_bug.cgi?id=1690069 https://bugzilla.redhat.com/show_bug.cgi?id=1691516
		index.Search = append(index.Search, "level=error.*Failed to reach target state.*");
		index.Search = append(index.Search, "waiting for Kubernetes API: context deadline exceeded");
		index.Search = append(index.Search, "failed to wait for bootstrapping to complete.*");
		index.Search = append(index.Search, "failed to initialize the cluster.*");
		*/
		index.Search = append(index.Search, "Container setup exited with code ., reason Error");
		//index.Search = append(index.Search, "Container setup in pod .* completed successfully");

		// Cluster-under-test issues
		index.Search = append(index.Search, "no providers available to validate pod");  // https://bugzilla.redhat.com/show_bug.cgi?id=1705102
		index.Search = append(index.Search, "Error deleting EBS volume .* since volume is currently attached");  // https://bugzilla.redhat.com/show_bug.cgi?id=1704356
		index.Search = append(index.Search, "clusteroperator/.* changed Degraded to True: .*");  // e.g. https://bugzilla.redhat.com/show_bug.cgi?id=1702829 https://bugzilla.redhat.com/show_bug.cgi?id=1702832
		index.Search = append(index.Search, "Cluster operator .* is still updating.*");  // e.g. https://bugzilla.redhat.com/show_bug.cgi?id=1700416
		index.Search = append(index.Search, "Pod .* is not healthy"); // e.g. https://bugzilla.redhat.com/show_bug.cgi?id=1700100
		/*
		index.Search = append(index.Search, "failed: .*oc new-app  should succeed with a --name of 58 characters");  // https://bugzilla.redhat.com/show_bug.cgi?id=1535099
		index.Search = append(index.Search, "failed to get logs from .*an error on the server");  // https://bugzilla.redhat.com/show_bug.cgi?id=1690168 closed as a dup of https://bugzilla.redhat.com/show_bug.cgi?id=1691055
		index.Search = append(index.Search, "openshift-apiserver OpenShift API is not responding to GET requests");  // https://bugzilla.redhat.com/show_bug.cgi?id=1701291
		index.Search = append(index.Search, "Cluster did not complete upgrade: timed out waiting for the condition");
		index.Search = append(index.Search, "Cluster did not acknowledge request to upgrade in a reasonable time: timed out waiting for the condition");  // https://bugzilla.redhat.com/show_bug.cgi?id=1703158 , also mentioned in https://bugzilla.redhat.com/show_bug.cgi?id=1701291#c1
		index.Search = append(index.Search, "failed: .*Cluster upgrade should maintain a functioning cluster");
		*/

		// generic patterns so you can hover to see details in the tooltip
		/*
		index.Search = append(index.Search, "error.*");
		index.Search = append(index.Search, "failed.*");
		index.Search = append(index.Search, "fatal.*");
		*/
		index.Search = append(index.Search, "failed: \\(.*");
	}

	switch req.FormValue("type") {
	case "junit":
		index.SearchType = "junit"
	case "build-log":
		index.SearchType = "build-log"
	case "all", "":
		index.SearchType = "all"
	default:
		return nil, fmt.Errorf("search must be 'junit', 'build-log', or 'all'")
	}

	if value := req.FormValue("name"); len(value) > 0 || mode == "chart" {
		if len(value) == 0 {
			value = "-e2e-"
		}
		var err error
		index.Job, err = regexp.Compile(value)
		if err != nil {
			return nil, fmt.Errorf("name is an invalid regular expression: %v", err)
		}
	}

	if value := req.FormValue("maxAge"); len(value) > 0 {
		maxAge, err := time.ParseDuration(value)
		if err != nil {
			return nil, fmt.Errorf("maxAge is an invalid duration: %v", err)
		} else if maxAge < 0 {
			return nil, fmt.Errorf("maxAge must be non-negative: %v", err)
		}
		index.MaxAge = maxAge
	}
	maxAge := o.MaxAge
	if maxAge == 0 {
		maxAge = 7 * 24 * time.Hour
	}
	if mode == "chart" && maxAge > 24*time.Hour {
		maxAge = 24 * time.Hour
	}
	if index.MaxAge == 0 || index.MaxAge > maxAge {
		index.MaxAge = maxAge
	}

	if context := req.FormValue("context"); len(context) > 0 {
		num, err := strconv.Atoi(context)
		if err != nil || num < -1 || num > 15 {
			return nil, fmt.Errorf("context must be a number between -1 and 15")
		}
		index.Context = num
	} else if mode == "text" {
		index.Context = 2
	}

	return index, nil
}

func intSelected(current, expected int) string {
	if current == expected {
		return "selected"
	}
	return ""
}

func durationSelected(current, expected time.Duration) string {
	if current == expected {
		return "selected"
	}
	return ""
}

func renderWithContext(ctx context.Context, w io.Writer, index *Index, generator CommandGenerator, start time.Time, resultMeta ResultMetadata) (int, error) {
	count := 0
	lineCount := 0
	var lastName string

	bw := bufio.NewWriterSize(w, 256*1024)
	err := executeGrep(ctx, generator, index, 30, func(name string, search string, matches []bytes.Buffer, moreLines int) {
		if count == 5 || count%50 == 0 {
			bw.Flush()
		}
		if lastName == name {
			fmt.Fprintf(bw, "\n&mdash;\n\n")
		} else {
			lastName = name
			if count > 0 {
				fmt.Fprintf(bw, `</pre></div>`)
			}
			count++

			var age string
			result, _ := resultMeta.MetadataFor(name)
			if !result.FailedAt.IsZero() {
				duration := start.Sub(result.FailedAt)
				age = " " + units.HumanDuration(duration)
			}

			fmt.Fprintf(bw, `<div class="mb-4">`)
			fmt.Fprintf(bw, `<h5 class="mb-3">%s from %s <a href="%s">%s #%d</a>%s</h5><pre class="small">`, template.HTMLEscapeString(result.FileType), template.HTMLEscapeString(result.Trigger), template.HTMLEscapeString(result.JobURI.String()), template.HTMLEscapeString(result.Name), result.Number, template.HTMLEscapeString(age))
		}

		// remove empty leading and trailing lines
		var lines [][]byte
		for _, m := range matches {
			line := bytes.TrimRight(m.Bytes(), " ")
			if len(line) == 0 {
				continue
			}
			lines = append(lines, line)
		}
		for i := len(lines) - 1; i >= 0; i-- {
			if len(lines[i]) != 0 {
				break
			}
			lines = lines[:i]
		}
		lineCount += len(lines)

		for _, line := range lines {
			template.HTMLEscape(bw, line)
			fmt.Fprintln(bw)
		}
		if moreLines > 0 {
			fmt.Fprintf(bw, "\n... %d lines not shown\n\n", moreLines)
		}
	})
	if count > 0 {
		fmt.Fprintf(bw, `</pre></div>`)
	}
	if err := bw.Flush(); err != nil {
		glog.Errorf("Unable to flush results buffer: %v", err)
	}
	return count, err
}

func renderSummary(ctx context.Context, w io.Writer, index *Index, generator CommandGenerator, start time.Time, resultMeta ResultMetadata) (int, error) {
	count := 0
	currentLines := 0
	var lastName string
	bw := bufio.NewWriterSize(w, 256*1024)
	fmt.Fprintf(bw, `<table class="table table-reponsive"><tbody><tr><th>Type</th><th>Job</th><th>Age</th><th># of hits</th></tr>`)
	err := executeGrep(ctx, generator, index, 30, func(name string, search string, matches []bytes.Buffer, moreLines int) {
		if count == 5 || count%50 == 0 {
			bw.Flush()
		}
		if lastName == name {
			// continue accumulating matches
		} else {
			lastName = name

			var age string
			result, _ := resultMeta.MetadataFor(name)
			if !result.FailedAt.IsZero() {
				duration := start.Sub(result.FailedAt)
				age = units.HumanDuration(duration) + " ago"
			}

			if result.JobURI == nil {
				glog.Errorf("no job URI for %q", name)
				return
			}

			if count > 0 {
				fmt.Fprintf(bw, "<td>%d</td>", currentLines)
				fmt.Fprintf(bw, `</tr>`)
				currentLines = 0
			}
			count++

			fmt.Fprintf(bw, `<tr>`)
			fmt.Fprintf(bw, `<td>%s</td><td><a href="%s">%s #%d</a></td><td>%s</td>`, template.HTMLEscapeString(result.FileType), template.HTMLEscapeString(result.JobURI.String()), template.HTMLEscapeString(result.Name), result.Number, template.HTMLEscapeString(age))
		}

		currentLines++
	})

	if count > 0 {
		fmt.Fprintf(bw, "<td>%d</td>", currentLines)
		fmt.Fprintf(bw, `</tr>`)
	}
	if err := bw.Flush(); err != nil {
		glog.Errorf("Unable to flush results buffer: %v", err)
	}
	return count, err
}

const htmlPageStart = `
<!DOCTYPE html>
<html>
<head>
<meta charset="UTF-8"><title>%s</title>
<link rel="stylesheet" href="https://stackpath.bootstrapcdn.com/bootstrap/4.1.3/css/bootstrap.min.css" integrity="sha384-MCw98/SFnGE8fJT3GXwEOngsV7Zt27NXFoaoApmYm81iuXoPkFOJwJ8ERdknLPMO" crossorigin="anonymous">
<meta name="viewport" content="width=device-width, initial-scale=1, shrink-to-fit=no">
<style>
</style>
</head>
<body>
<div class="container-fluid">
`

const htmlPageEnd = `
</div>
</body>
</html>
`

const htmlIndexForm = `
<form class="form mt-4 mb-4" method="GET">
	<div class="input-group input-group-lg"><input autocomplete="off" autofocus name="search" class="form-control col-auto" value="%s" placeholder="Search OpenShift CI failures by entering a regex search ...">
	<select name="maxAge" class="form-control col-1" onchange="this.form.submit();">%s</select>
	<select name="context" class="form-control col-1" onchange="this.form.submit();">%s</select>
	<select name="type" class="form-control col-1" onchange="this.form.submit();">%s</select>
	<input class="btn" type="submit" value="Search">
	</div>
</form>
`

const htmlEmptyPage = `
<div class="ml-3" style="margin-top: 3rem; color: #666;">
<p>Find JUnit test failures from <a href="/config">a subset of CI jobs</a> in <a href="https://deck-ci.svc.ci.openshift.org">OpenShift CI</a>.</p>
<p>The search input will use <a href="https://docs.rs/regex/0.2.5/regex/#syntax">ripgrep regular-expression patterns</a>.</p>
<p>Searches are case-insensitive (using ripgrep "smart casing")</p>
<p>Examples:
<ul>
<li><code>timeout</code> - all JUnit failures with 'timeout' in the result</li>
<li><code>status code \d{3}\s</code> - all failures that contain 'status code' followed by a 3 digit number</li>
</ul>
<p>You can alter the age of results to search with the dropdown next to the search bar. Note that older results are pruned and may not be available after 14 days.</p>
<p>The amount of surrounding text returned with each match can be changed, including none.
<p>Currently indexing %s across %d entries</p>
</div>
`
