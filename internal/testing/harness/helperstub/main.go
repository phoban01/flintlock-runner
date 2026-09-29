// Command helperstub stands in for gitlab-runner-helper in the guests of the
// end-to-end harness. The fake Hosts run a Job's commands on the machine the
// harness runs on, which has no gitlab-runner-helper, so the Profile's
// helper path (EX-018) names this instead. It implements the subcommands
// that the artifact and cache steps call, with the arguments that
// gitlab-runner's shell writes: artifacts-uploader and artifacts-downloader
// against the fake GitLab's job artifacts API, and cache-archiver and
// cache-extractor against the pre-signed URLs of the distributed cache. It
// also answers --version, with which the scripts check that it is there.
// Anything else fails. It uses the standard library only, so the harness
// builds it in a moment.
package main

import (
	"archive/zip"
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func main() {
	if len(os.Args) < 2 {
		fail(errors.New("usage: helperstub artifacts-uploader|artifacts-downloader|cache-archiver|cache-extractor [flags]"))
	}
	flags := parse(os.Args[2:])
	var err error
	switch os.Args[1] {
	case "--version":
		fmt.Println("gitlab-runner-helper (flintlock-runner harness stand-in)")
	case "artifacts-uploader":
		err = upload(flags)
	case "artifacts-downloader":
		err = download(flags)
	case "cache-archiver":
		err = archiveCache(flags)
	case "cache-extractor":
		err = extractCache(flags)
	default:
		err = fmt.Errorf("the harness's gitlab-runner-helper stand-in does not implement %q", os.Args[1])
	}
	if err != nil {
		fail(err)
	}
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "gitlab-runner-helper (harness stand-in):", err)
	os.Exit(1)
}

// parse reads `--name value` pairs, repeated names collecting every value;
// a flag without a value, such as --untracked, is recorded empty.
func parse(args []string) map[string][]string {
	out := map[string][]string{}
	for i := 0; i < len(args); i++ {
		name, ok := strings.CutPrefix(args[i], "--")
		if !ok {
			continue
		}
		if k, v, found := strings.Cut(name, "="); found {
			out[k] = append(out[k], v)
			continue
		}
		if i+1 < len(args) && !strings.HasPrefix(args[i+1], "--") {
			out[name] = append(out[name], args[i+1])
			i++
			continue
		}
		out[name] = append(out[name], "")
	}
	return out
}

func first(flags map[string][]string, name string) string {
	if v := flags[name]; len(v) > 0 {
		return v[0]
	}
	return ""
}

var client = &http.Client{Timeout: time.Minute}

// artifactsURL is the job artifacts endpoint of the GitLab at base.
func artifactsURL(base, id string) string {
	return strings.TrimSuffix(base, "/") + "/api/v4/jobs/" + url.PathEscape(id) + "/artifacts"
}

// zipPaths zips every file under each pattern, relative to the working
// directory and matched as a glob, and returns the archive and the number
// of files in it.
func zipPaths(patterns []string) ([]byte, int, error) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	files := 0
	for _, pattern := range patterns {
		matches, err := filepath.Glob(pattern)
		if err != nil {
			return nil, 0, err
		}
		for _, m := range matches {
			err := filepath.WalkDir(m, func(path string, d fs.DirEntry, err error) error {
				if err != nil || d.IsDir() {
					return err
				}
				data, err := os.ReadFile(path)
				if err != nil {
					return err
				}
				w, err := zw.Create(filepath.ToSlash(path))
				if err != nil {
					return err
				}
				files++
				_, err = w.Write(data)
				return err
			})
			if err != nil {
				return nil, 0, err
			}
		}
	}
	if err := zw.Close(); err != nil {
		return nil, 0, err
	}
	return buf.Bytes(), files, nil
}

// unzipInto extracts a zip archive into the working directory and returns
// the number of entries.
func unzipInto(data []byte) (int, error) {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return 0, err
	}
	for _, f := range zr.File {
		dst := filepath.Clean(f.Name)
		if filepath.IsAbs(dst) || strings.HasPrefix(dst, "..") {
			return 0, fmt.Errorf("archive entry %q leaves the working directory", f.Name)
		}
		if strings.HasSuffix(f.Name, "/") {
			if err := os.MkdirAll(dst, 0o755); err != nil {
				return 0, err
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return 0, err
		}
		rc, err := f.Open()
		if err != nil {
			return 0, err
		}
		content, err := io.ReadAll(rc)
		_ = rc.Close()
		if err != nil {
			return 0, err
		}
		if err := os.WriteFile(dst, content, 0o644); err != nil {
			return 0, err
		}
	}
	return len(zr.File), nil
}

// upload zips every --path and posts the archive as the Job's artifact.
func upload(flags map[string][]string) error {
	archive, files, err := zipPaths(flags["path"])
	if err != nil {
		return err
	}
	if files == 0 {
		return fmt.Errorf("no files to upload for %v", flags["path"])
	}
	name := first(flags, "name")
	if name == "" {
		name = "artifacts"
	}
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	part, err := mw.CreateFormFile("file", name+".zip")
	if err != nil {
		return err
	}
	if _, err := part.Write(archive); err != nil {
		return err
	}
	if err := mw.Close(); err != nil {
		return err
	}
	q := url.Values{}
	q.Set("artifact_format", valueOr(first(flags, "artifact-format"), "zip"))
	q.Set("artifact_type", valueOr(first(flags, "artifact-type"), "archive"))
	if e := first(flags, "expire-in"); e != "" {
		q.Set("expire_in", e)
	}
	req, err := http.NewRequest(http.MethodPost, artifactsURL(first(flags, "url"), first(flags, "id"))+"?"+q.Encode(), &body)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("JOB-TOKEN", first(flags, "token"))
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusCreated {
		msg, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("uploading artifacts: %s: %s", resp.Status, msg)
	}
	fmt.Printf("Uploading artifacts as \"archive\" to coordinator... %s (%d files)\n", resp.Status, files)
	return nil
}

// download fetches a Job's archive and extracts it into the working
// directory.
func download(flags map[string][]string) error {
	req, err := http.NewRequest(http.MethodGet, artifactsURL(first(flags, "url"), first(flags, "id")), nil)
	if err != nil {
		return err
	}
	req.Header.Set("JOB-TOKEN", first(flags, "token"))
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("downloading artifacts of job %s: %s: %s", first(flags, "id"), resp.Status, data)
	}
	n, err := unzipInto(data)
	if err != nil {
		return err
	}
	fmt.Printf("Downloading artifacts from coordinator... ok (%d files)\n", n)
	return nil
}

// archiveCache zips every --path into --file and uploads it with a PUT to
// the pre-signed --url, with each --header.
func archiveCache(flags map[string][]string) error {
	archive, files, err := zipPaths(flags["path"])
	if err != nil {
		return err
	}
	if file := first(flags, "file"); file != "" {
		if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(file, archive, 0o644); err != nil {
			return err
		}
	}
	target := first(flags, "url")
	if target == "" {
		return errors.New("cache-archiver: no --url to upload the cache to")
	}
	req, err := http.NewRequest(http.MethodPut, target, bytes.NewReader(archive))
	if err != nil {
		return err
	}
	for _, h := range flags["header"] {
		if k, v, ok := strings.Cut(h, ":"); ok {
			req.Header.Add(strings.TrimSpace(k), strings.TrimSpace(v))
		}
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode/100 != 2 {
		msg, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("uploading the cache: %s: %s", resp.Status, msg)
	}
	fmt.Printf("Uploading cache.zip to the distributed cache... %s (%d files)\n", resp.Status, files)
	return nil
}

// extractCache downloads the archive at the pre-signed --url into --file
// and extracts it into the working directory. A cache that does not exist
// yet is a failure, which the generated script reports as a warning.
func extractCache(flags map[string][]string) error {
	source := first(flags, "url")
	if source == "" {
		return errors.New("cache-extractor: no --url to download the cache from")
	}
	resp, err := client.Get(source)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode == http.StatusNotFound {
		return errors.New("the cache does not exist yet")
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("downloading the cache: %s: %s", resp.Status, data)
	}
	if file := first(flags, "file"); file != "" {
		if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(file, data, 0o644); err != nil {
			return err
		}
	}
	n, err := unzipInto(data)
	if err != nil {
		return err
	}
	fmt.Printf("Downloading cache.zip from the distributed cache... ok (%d files)\n", n)
	return nil
}

func valueOr(v, def string) string {
	if v == "" {
		return def
	}
	return v
}
