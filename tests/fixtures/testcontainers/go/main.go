package main

import (
	"context"
	"fmt"
	"os"

	"github.com/testcontainers/testcontainers-go"
)

func main() {
	image := os.Getenv("BWRAP_AGENT_TEST_IMAGE")
	if image == "" {
		panic("BWRAP_AGENT_TEST_IMAGE is required")
	}
	container, err := testcontainers.GenericContainer(context.Background(), testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image: image,
			Cmd:   []string{"/bin/sh", "-ec", "trap 'exit 0' TERM; while :; do sleep 1; done"},
		},
		Started: true,
	})
	if err != nil {
		panic(err)
	}
	defer container.Terminate(context.Background())
	if _, err := container.ContainerIP(context.Background()); err != nil {
		panic(err)
	}
	fmt.Println("testcontainers-go-ok")
}
