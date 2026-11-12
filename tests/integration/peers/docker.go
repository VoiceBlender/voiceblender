package peers

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// ContainerLabel marks every container an interop endpoint starts, so leftovers
// from an aborted run can be found and removed.
const ContainerLabel = "vb-interop"

// RunSpec describes a container to start.
type RunSpec struct {
	Name    string
	Image   string
	Volumes map[string]string // host path -> container path
	Env     map[string]string
	Args    []string // after the image
}

// Container is a running docker container.
type Container struct {
	ID   string
	Name string
}

func docker(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "docker", args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return stdout.String(), fmt.Errorf("docker %s: %w: %s", args[0], err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}

// DockerAvailable reports why docker cannot be used, or nil.
func DockerAvailable() error {
	if _, err := exec.LookPath("docker"); err != nil {
		return fmt.Errorf("docker CLI not found: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if _, err := docker(ctx, "info", "--format", "{{.ServerVersion}}"); err != nil {
		return fmt.Errorf("docker daemon not reachable: %w", err)
	}
	return nil
}

// RemoveStale force-removes containers named name left by an earlier run.
func RemoveStale(name string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, _ := docker(ctx, "ps", "-aq", "--filter", "label="+ContainerLabel, "--filter", "name=^/"+name+"$")
	for _, id := range strings.Fields(out) {
		docker(ctx, "rm", "-f", id)
	}
}

// Run starts spec on the host network. The image is pulled if missing.
func Run(spec RunSpec) (*Container, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	args := []string{"run", "-d", "--network", "host", "--label", ContainerLabel + "=1", "--name", spec.Name}
	for host, ctr := range spec.Volumes {
		args = append(args, "-v", host+":"+ctr)
	}
	for k, v := range spec.Env {
		args = append(args, "-e", k+"="+v)
	}
	args = append(args, spec.Image)
	args = append(args, spec.Args...)
	out, err := docker(ctx, args...)
	if err != nil {
		return nil, err
	}
	return &Container{ID: strings.TrimSpace(out), Name: spec.Name}, nil
}

// Logs returns the last lines of the container's output.
func (c *Container) Logs(tail int) string {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "docker", "logs", "--tail", fmt.Sprint(tail), c.ID)
	out, _ := cmd.CombinedOutput()
	return string(out)
}

// Running reports whether the container is still up.
func (c *Container) Running() bool {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	out, err := docker(ctx, "inspect", "-f", "{{.State.Running}}", c.ID)
	return err == nil && strings.TrimSpace(out) == "true"
}

// Exec runs a command inside the container.
func (c *Container) Exec(args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return docker(ctx, append([]string{"exec", c.ID}, args...)...)
}

// Remove stops and deletes the container.
func (c *Container) Remove() error {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	_, err := docker(ctx, "rm", "-f", c.ID)
	return err
}
