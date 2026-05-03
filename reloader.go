package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"
	"github.com/docker/docker/pkg/stdcopy"
)

// Reloader runs `nginx -t && nginx -s reload` inside the lancache container
// over the Docker API. It lazily creates a Docker client on first use so the
// monitor can boot even if /var/run/docker.sock isn't mounted yet.
type Reloader struct {
	host          string
	containerName string

	mu  sync.Mutex
	cli *client.Client
}

// NewReloader returns a Reloader configured for the given Docker host
// (e.g. "unix:///var/run/docker.sock") and target container name.
func NewReloader(host, containerName string) *Reloader {
	return &Reloader{host: host, containerName: containerName}
}

func (r *Reloader) ensureClient() (*client.Client, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.cli != nil {
		return r.cli, nil
	}
	cli, err := client.NewClientWithOpts(
		client.WithHost(r.host),
		client.WithAPIVersionNegotiation(),
	)
	if err != nil {
		return nil, fmt.Errorf("docker client: %w", err)
	}
	r.cli = cli
	return cli, nil
}

// ReloadResult is what the UI shows after an Apply & reload.
type ReloadResult struct {
	Stdout   string
	Stderr   string
	ExitCode int
	OK       bool
}

// Combined returns stdout+stderr concatenated, useful for the UI panel.
func (rr ReloadResult) Combined() string {
	if rr.Stderr == "" {
		return rr.Stdout
	}
	if rr.Stdout == "" {
		return rr.Stderr
	}
	return rr.Stdout + "\n" + rr.Stderr
}

// Reload runs `nginx -t && nginx -s reload` inside the lancache container.
// On any non-zero exit, the caller should treat the new config as invalid
// and roll back.
func (r *Reloader) Reload(ctx context.Context) (ReloadResult, error) {
	cli, err := r.ensureClient()
	if err != nil {
		return ReloadResult{}, err
	}

	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	exec, err := cli.ContainerExecCreate(ctx, r.containerName, container.ExecOptions{
		Cmd:          []string{"sh", "-c", "nginx -t && nginx -s reload"},
		AttachStdout: true,
		AttachStderr: true,
	})
	if err != nil {
		return ReloadResult{}, fmt.Errorf("exec create on %q: %w", r.containerName, err)
	}

	attach, err := cli.ContainerExecAttach(ctx, exec.ID, container.ExecAttachOptions{})
	if err != nil {
		return ReloadResult{}, fmt.Errorf("exec attach: %w", err)
	}
	defer attach.Close()

	var stdout, stderr bytes.Buffer
	if _, err := stdcopy.StdCopy(&stdout, &stderr, attach.Reader); err != nil && err != io.EOF {
		return ReloadResult{}, fmt.Errorf("read exec output: %w", err)
	}

	// Poll until the exec has actually finished — Attach can return before the
	// process exits, leaving ExitCode at 0.
	var exitCode int
	for {
		insp, err := cli.ContainerExecInspect(ctx, exec.ID)
		if err != nil {
			return ReloadResult{}, fmt.Errorf("exec inspect: %w", err)
		}
		if !insp.Running {
			exitCode = insp.ExitCode
			break
		}
		select {
		case <-ctx.Done():
			return ReloadResult{}, ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}

	return ReloadResult{
		Stdout:   stdout.String(),
		Stderr:   stderr.String(),
		ExitCode: exitCode,
		OK:       exitCode == 0,
	}, nil
}
