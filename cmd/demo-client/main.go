package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

func main() {
	file := flag.String("file", "", "input media file")
	base := flag.String("server", "http://localhost:8080", "server URL")
	flag.Parse()
	if *file == "" {
		panic("--file is required")
	}
	f, e := os.Open(*file)
	if e != nil {
		panic(e)
	}
	defer f.Close()
	st, _ := f.Stat()
	whole := sha256.New()
	if _, e = io.Copy(whole, f); e != nil {
		panic(e)
	}
	if _, e = f.Seek(0, io.SeekStart); e != nil {
		panic(e)
	}
	body, _ := json.Marshal(map[string]any{"input": map[string]any{"filename": filepath.Base(*file), "size": st.Size(), "sha256": hex.EncodeToString(whole.Sum(nil))}, "output": map[string]any{"preset": "web-1080p"}})
	r, e := http.Post(*base+"/v1/jobs", "application/json", bytes.NewReader(body))
	if e != nil {
		panic(e)
	}
	defer r.Body.Close()
	var j struct {
		ID     string `json:"id"`
		Upload struct {
			Chunk int64 `json:"chunk_size"`
		} `json:"upload"`
	}
	if r.StatusCode != 201 {
		io.Copy(os.Stderr, r.Body)
		os.Exit(1)
	}
	json.NewDecoder(r.Body).Decode(&j)
	for n, off := 0, int64(0); off < st.Size(); n, off = n+1, off+j.Upload.Chunk {
		size := j.Upload.Chunk
		if st.Size()-off < size {
			size = st.Size() - off
		}
		// Hash then rewind a bounded SectionReader: memory stays O(1).
		h := sha256.New()
		if _, e = io.Copy(h, io.NewSectionReader(f, off, size)); e != nil {
			panic(e)
		}
		q, e := http.NewRequest("PUT", *base+"/v1/jobs/"+j.ID+"/chunks/"+strconv.Itoa(n), io.NewSectionReader(f, off, size))
		if e != nil {
			panic(e)
		}
		q.ContentLength = size
		q.Header.Set("X-Chunk-SHA256", hex.EncodeToString(h.Sum(nil)))
		x, e := http.DefaultClient.Do(q)
		if e != nil || x.StatusCode != 200 {
			panic("chunk upload failed")
		}
		x.Body.Close()
		fmt.Printf("uploaded chunk %d\n", n)
	}
	x, e := http.Post(*base+"/v1/jobs/"+j.ID+"/complete", "application/json", nil)
	if e != nil || x.StatusCode != 202 {
		panic("complete failed")
	}
	x.Body.Close()
	for {
		time.Sleep(time.Second)
		x, _ = http.Get(*base + "/v1/jobs/" + j.ID)
		var z struct {
			State string `json:"state"`
		}
		json.NewDecoder(x.Body).Decode(&z)
		x.Body.Close()
		fmt.Println(z.State)
		if z.State == "COMPLETED" || z.State == "FAILED" {
			break
		}
	}
}
