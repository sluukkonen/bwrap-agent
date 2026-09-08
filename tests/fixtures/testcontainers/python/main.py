import os

from testcontainers.core.container import DockerContainer


image = os.environ.get("BWRAP_AGENT_TEST_IMAGE")
if not image:
    raise RuntimeError("BWRAP_AGENT_TEST_IMAGE is required")

container = DockerContainer(image).with_command(
    "/bin/sh -ec \"trap 'exit 0' TERM; while :; do sleep 1; done\""
)
container.start()
try:
    if not container.get_wrapped_container().id:
        raise RuntimeError("Testcontainers did not return a container ID")
    print("testcontainers-python-ok")
finally:
    container.stop()
