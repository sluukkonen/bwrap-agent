package integration;

import java.net.InetAddress;
import org.testcontainers.containers.GenericContainer;
import org.testcontainers.utility.DockerImageName;

public final class Main {
    private Main() {
    }

    public static void main(String[] args) throws Exception {
        String image = System.getenv("BWRAP_AGENT_TEST_IMAGE");
        if (image == null || image.isEmpty()) {
            throw new IllegalStateException("BWRAP_AGENT_TEST_IMAGE is required");
        }
        String hostname = InetAddress.getLocalHost().getHostName();
        if (!hostname.startsWith("agent-")) {
            throw new IllegalStateException("Unexpected sandbox hostname: " + hostname);
        }
        try (GenericContainer<?> container = new GenericContainer<>(DockerImageName.parse(image))) {
            container.withCommand("/bin/sh", "-ec", "trap 'exit 0' TERM; while :; do sleep 1; done");
            container.start();
            if (!container.isRunning()) {
                throw new IllegalStateException("Testcontainers did not start a container");
            }
            System.out.println("testcontainers-java-ok");
        }
    }
}
