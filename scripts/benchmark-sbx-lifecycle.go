// benchmark-sbx-lifecycle measures the raw Docker Engine lifecycle only.
// It deliberately does not measure e2b-local's envd and tunnel readiness path.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"sort"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/strslice"
	"github.com/docker/docker/client"
)

type target struct {
	name string
	host string
}

func main() {
	image := flag.String("image", "e2b-local/sbx-envd:dev", "image present in both Docker engines")
	localHost := flag.String("local-docker-host", os.Getenv("DOCKER_HOST"), "local Docker endpoint; empty uses the Docker SDK default")
	sbxHost := flag.String("sbx-docker-host", "unix://"+os.Getenv("HOME")+"/.sbx/run/d/docker.sock", "authenticated sbx Docker endpoint")
	runs := flag.Int("runs", 5, "number of recorded runs per target")
	timeout := flag.Duration("timeout", 2*time.Minute, "timeout for each lifecycle run")
	flag.Parse()

	if *runs < 1 {
		fatalf("runs must be at least one")
	}

	for _, item := range []target{{name: "local", host: *localHost}, {name: "sbx", host: *sbxHost}} {
		measureTarget(item, *image, *runs, *timeout)
	}
}

func measureTarget(item target, image string, runs int, timeout time.Duration) {
	clientOpts := []client.Opt{client.WithAPIVersionNegotiation()}
	if item.host != "" {
		clientOpts = append([]client.Opt{client.WithHost(item.host)}, clientOpts...)
	}
	dockerClient, err := client.NewClientWithOpts(clientOpts...)
	if err != nil {
		fatalf("create %s Docker client: %v", item.name, err)
	}
	defer dockerClient.Close()

	checkCtx, cancel := context.WithTimeout(context.Background(), timeout)
	_, _, err = dockerClient.ImageInspectWithRaw(checkCtx, image)
	cancel()
	if err != nil {
		fatalf("inspect %s image %q: %v", item.name, image, err)
	}

	// The warm-up is intentionally excluded: it may allocate a VM or lazily load
	// image state for the target engine.
	if _, err := lifecycle(dockerClient, item.name, image, timeout); err != nil {
		fatalf("warm up %s: %v", item.name, err)
	}

	results := make([]time.Duration, 0, runs)
	for index := 0; index < runs; index++ {
		elapsed, err := lifecycle(dockerClient, item.name, image, timeout)
		if err != nil {
			fatalf("%s run %d: %v", item.name, index+1, err)
		}
		results = append(results, elapsed)
	}

	for index, elapsed := range results {
		fmt.Printf("%s run %d: %s\n", item.name, index+1, elapsed.Round(time.Millisecond))
	}
	fmt.Printf("%s p50: %s, mean: %s\n", item.name, median(results).Round(time.Millisecond), mean(results).Round(time.Millisecond))
}

func lifecycle(dockerClient *client.Client, targetName, image string, timeout time.Duration) (time.Duration, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	name := fmt.Sprintf("e2b-local-bench-%s-%d", targetName, time.Now().UnixNano())
	startedAt := time.Now()
	created, err := dockerClient.ContainerCreate(ctx, &container.Config{
		Image:      image,
		Entrypoint: strslice.StrSlice{"/bin/sh"},
		Cmd:        strslice.StrSlice{"-c", "sleep 30"},
	}, nil, nil, nil, name)
	if err != nil {
		return 0, fmt.Errorf("create container: %w", err)
	}
	defer func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), timeout)
		defer cleanupCancel()
		_ = dockerClient.ContainerRemove(cleanupCtx, created.ID, container.RemoveOptions{Force: true})
	}()

	if err := dockerClient.ContainerStart(ctx, created.ID, container.StartOptions{}); err != nil {
		return 0, fmt.Errorf("start container: %w", err)
	}
	return time.Since(startedAt), nil
}

func median(values []time.Duration) time.Duration {
	ordered := append([]time.Duration(nil), values...)
	sort.Slice(ordered, func(left, right int) bool { return ordered[left] < ordered[right] })
	middle := len(ordered) / 2
	if len(ordered)%2 == 1 {
		return ordered[middle]
	}
	return (ordered[middle-1] + ordered[middle]) / 2
}

func mean(values []time.Duration) time.Duration {
	var total time.Duration
	for _, value := range values {
		total += value
	}
	return total / time.Duration(len(values))
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}
