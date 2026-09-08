import { GenericContainer } from "testcontainers";

const image = process.env.BWRAP_AGENT_TEST_IMAGE;
if (!image) {
  throw new Error("BWRAP_AGENT_TEST_IMAGE is required");
}

const container = await new GenericContainer(image)
  .withCommand(["/bin/sh", "-ec", "trap 'exit 0' TERM; while :; do sleep 1; done"])
  .start();
try {
  if (!container.getId()) {
    throw new Error("Testcontainers did not return a container ID");
  }
  console.log("testcontainers-node-ok");
} finally {
  await container.stop();
}
