package main

import (
	// git "github.com/go-git/go-git/v5"
	"archive/zip"
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/fatih/color"
	"github.com/google/go-github/v60/github"
)

type highlight struct {
	phrase string
	color  *color.Color
}

var (
	muteLines = []string{
		" level=info ",
		"--:--:--",             // curl stuff
		"Average Speed   Time", // curl stuff
		"Dload  Upload",        // curl stuff
		"GET / HTTP/1.1",       // curl stuff
	}
	mutePairs = [][]string{
		{"<!DOCTYPE html>", "</html>"}, // verbose curl output
	}
	highlightKeywords = []highlight{
		{"error", color.New(color.BgRed).Add(color.FgWhite)},
		{"error trace:", color.New(color.BgBlue).Add(color.FgWhite)},
	}
	reset = color.New(color.Reset)
)

func main() {
	if len(os.Args) < 4 {
		fmt.Printf("usage: %s <owner> <repo> <pr>", os.Args[0])
		return
	}

	prID, err := strconv.Atoi(os.Args[3])
	if err != nil {
		fmt.Printf("not a PR ID")
	}

	ownerName := os.Args[1]
	repoName := os.Args[2]

	tkBytes, err := os.ReadFile("./token.txt")
	if err != nil {
		panic(err)
	}
	tk := strings.TrimSpace(string(tkBytes))

	ctx := context.Background()
	client := github.NewClient(nil).WithAuthToken(tk)
	pr, _, err := client.PullRequests.Get(ctx, ownerName, repoName, prID)
	if err != nil {
		panic(err)
	}

	fmt.Printf("✳️  PR: %s\n", *pr.Title)

	checkRes, _, err := client.Checks.ListCheckRunsForRef(ctx, ownerName, repoName, *pr.Head.Ref, nil)
	if err != nil {
		panic(err)
	}

	for _, run := range checkRes.CheckRuns {
		if run.Conclusion == nil || *run.Conclusion != "failure" {
			continue
		}

		workflowJob, _, err := client.Actions.GetWorkflowJobByID(ctx, ownerName, repoName, run.GetID())
		if err != nil {
			fmt.Printf("> failed to get workflow job: %v\n", err)
			continue
		}

		logFilename := filepath.Join("./cache", fmt.Sprintf("log_%d.zip", *workflowJob.RunID))
		if _, err := os.Stat(logFilename); err != nil {
			fmt.Printf("⬇️  Downloading log file\n")
			logURL, _, err := client.Actions.GetWorkflowRunLogs(ctx, ownerName, repoName, *workflowJob.RunID, 5)
			if err != nil {
				fmt.Printf("> failed to get logs url: %v\n", err)
				continue
			}

			f, err := os.Create(logFilename)
			if err != nil {
				panic(err)
			}

			req, _ := http.NewRequest("GET", logURL.String(), nil)
			res, err := client.Do(ctx, req, f)
			f.Close()
			if err != nil {
				fmt.Printf("> failed to download logs: %v\n", err)
				os.Remove(logFilename)
				continue
			}

			if res.StatusCode != 200 {
				fmt.Printf("> unexpected status code for log request: %d", res.StatusCode)
				continue
			}
		}

		for _, step := range workflowJob.Steps {
			if step.Conclusion == nil || *step.Conclusion != "failure" {
				continue
			}
			fmt.Printf("🔄  Job %q (Step %d)\n", *step.Name, *step.Number)
			ParseGoLogs(logFilename, *workflowJob.Name, *step.Number)
		}
	}
}

const (
	PrefixName      = "=== NAME  "
	PrefixRun       = "=== RUN   "
	PrefixPause     = "=== PAUSE "
	PrefixCont      = "=== CONT  "
	PrefixFail      = "--- FAIL: "
	PrefixPass      = "--- PASS: "
	PrefixResult    = "--- "
	PrefixSubResult = "    --- "
	PrefixStage     = "=== "
)

type stream struct {
	*bytes.Buffer
	muted      bool
	unmuteWith []byte
}

func parseGoLogs(r io.Reader) {
	scanContents := make(map[string]*stream)
	scanner := bufio.NewScanner(r)

	contentArr := make([]string, 0)

	var curStream *stream
nextLine:
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) > 29 && line[28] == ' ' {
			// strip timestamp
			line = line[29:]
		}
		// fmt.Printf("<%s>\n", string(line))
		if !bytes.HasPrefix(line, []byte(PrefixStage)) && !bytes.HasPrefix(line, []byte(PrefixResult)) && !bytes.HasPrefix(line, []byte(PrefixSubResult)) {
			if curStream != nil {
				if len(bytes.TrimSpace(line)) == 0 {
					continue
				}

				if curStream.muted {
					if bytes.Contains(line, curStream.unmuteWith) {
						curStream.muted = false
					}
					continue nextLine
				}

				for _, m := range muteLines {
					if bytes.Contains(line, []byte(m)) {
						continue nextLine
					}
				}

				for _, m := range mutePairs {
					if bytes.Contains(line, []byte(m[0])) {
						curStream.muted = true
						curStream.unmuteWith = []byte(m[1])
						continue nextLine
					}
				}

				if len(line) > 400 {
					// abbrev
					line = append(line[:200], []byte("...")...)
				}

				colored := false
				for _, h := range highlightKeywords {
					if strings.Contains(strings.ToLower(string(line)), h.phrase) {
						h.color.SetWriter(curStream)
						colored = true
					}
				}

				curStream.Write(line)

				if colored {
					reset.SetWriter(curStream)
				}

				curStream.WriteRune('\n')
			}
			continue
		}
		if bytes.HasPrefix(line, []byte(PrefixStage)) {
			name := strings.Split(string(line)[len(PrefixName):], " ")
			if _, ok := scanContents[name[0]]; !ok {
				scanContents[name[0]] = &stream{
					Buffer:     bytes.NewBuffer(nil),
					muted:      false,
					unmuteWith: nil,
				}
				contentArr = append(contentArr, name[0])
			}
			curStream = scanContents[name[0]]
			continue
		}
		// trim subtest prefix
		if bytes.HasPrefix(line, []byte(PrefixSubResult)) {
			line = bytes.TrimPrefix(line, []byte("    "))
		}
		if bytes.HasPrefix(line, []byte(PrefixResult)) {
			tr := strings.Split(string(line), " ")
			if len(tr) < 3 {
				continue
			}
			if !strings.HasSuffix(tr[1], ":") {
				continue
			}
			stream, ok := scanContents[tr[2]]
			if !ok {
				// stream not found
				continue
			}

			curStream = nil

			// Delete all but fails
			if !bytes.HasPrefix(line, []byte(PrefixFail)) {
				delete(scanContents, tr[2])
				continue
			}

			// Save result
			stream.Write(line)
			stream.WriteRune('\n')
		}
	}

	for _, testName := range contentArr {
		output, ok := scanContents[testName]
		if !ok {
			continue
		}
		fmt.Printf("⚠️  Test %s\n%s\n\n\n", testName, output.String())
	}
}

func ParseGoLogs(fn string, step string, id int64) ([]byte, error) {
	fnprefix := fmt.Sprintf("%s/%d_", step, id)
	f, err := os.Open(fn)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	s, _ := f.Stat()
	rd, _ := zip.NewReader(f, s.Size())
	for _, f := range rd.File {
		if !strings.HasPrefix(f.Name, fnprefix) {
			continue
		}
		xf, err := f.Open()
		if err != nil {
			return nil, fmt.Errorf("opening file %q in zip archive: %w", f.Name, err)
		}
		defer xf.Close()
		parseGoLogs(xf)
		return nil, err
	}
	return nil, os.ErrNotExist
}

func GetLogFromFile(fn string, step string, id int64) ([]byte, error) {
	fnprefix := fmt.Sprintf("%s/%d_", step, id)
	f, err := os.Open(fn)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	s, _ := f.Stat()
	rd, _ := zip.NewReader(f, s.Size())
	for _, f := range rd.File {
		if !strings.HasPrefix(f.Name, fnprefix) {
			continue
		}
		xf, err := f.Open()
		if err != nil {
			return nil, fmt.Errorf("opening file %q in zip archive: %w", f.Name, err)
		}
		defer xf.Close()
		data, err := io.ReadAll(xf)
		return data, err
	}
	return nil, os.ErrNotExist
}
