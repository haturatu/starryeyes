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

type jobStatus struct {
	ID     string `json:"id"`
	State  string `json:"state"`
	Upload *struct {
		Chunk int64 `json:"chunk_size"`
	} `json:"upload"`
}

type chunksStatus struct {
	ChunkSize int64 `json:"chunk_size"`
	Chunks    []struct {
		Number int `json:"number"`
	} `json:"chunks"`
}

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
	request, e := http.NewRequest(http.MethodPost, *base+"/v1/jobs", bytes.NewReader(body))
	if e != nil {
		panic(e)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", fmt.Sprintf("demo-%d-%d", time.Now().UnixNano(), os.Getpid()))
	r, e := http.DefaultClient.Do(request)
	if e != nil {
		panic(e)
	}
	defer r.Body.Close()
	var j jobStatus
	if r.StatusCode != 201 {
		io.Copy(os.Stderr, r.Body)
		os.Exit(1)
	}
	json.NewDecoder(r.Body).Decode(&j)
	r.Body.Close()
	for j.State == "PENDING" {
		time.Sleep(time.Second)
		j = getJob(*base, j.ID)
	}
	if j.State != "ADMITTED" && j.State != "UPLOADING" {
		panic("job is not resumable: " + j.State)
	}
	x, e := http.Get(*base + "/v1/jobs/" + j.ID + "/chunks")
	if e != nil || x.StatusCode != http.StatusOK {
		panic("list chunks failed")
	}
	var chunks chunksStatus
	if e = json.NewDecoder(x.Body).Decode(&chunks); e != nil {
		panic(e)
	}
	x.Body.Close()
	verified := map[int]bool{}
	for _, chunk := range chunks.Chunks {
		verified[chunk.Number] = true
	}
	for n, off := 0, int64(0); off < st.Size(); n, off = n+1, off+chunks.ChunkSize {
		if verified[n] {
			fmt.Printf("skipped verified chunk %d\n", n)
			continue
		}
		size := chunks.ChunkSize
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
	x, e = http.Post(*base+"/v1/jobs/"+j.ID+"/complete", "application/json", nil)
	if e != nil || x.StatusCode != 202 {
		panic("complete failed")
	}
	x.Body.Close()
	for {
		time.Sleep(time.Second)
		j = getJob(*base, j.ID)
		fmt.Println(j.State)
		if j.State == "COMPLETED" || j.State == "FAILED" {
			break
		}
	}
}

func getJob(base, id string) jobStatus {
	response, err := http.Get(base + "/v1/jobs/" + id)
	if err != nil || response.StatusCode != http.StatusOK {
		panic("get job failed")
	}
	defer response.Body.Close()
	var job jobStatus
	if err = json.NewDecoder(response.Body).Decode(&job); err != nil {
		panic(err)
	}
	return job
}
