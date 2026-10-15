// Package docker is the Docker sandbox of the SDK (A-60): one long-lived container
// per sandbox on a named volume,
// driven through the docker CLI behind a Runner seam. There is no Docker client
// library and no Docker socket mount.
//
//	shared sandbox   aec-sandbox-shared        /workspace/agents/<id> per agent (there is
//	                                           no /workspace/shared)
//	own sandbox      aec-sandbox-own-<agentId> the whole /workspace
//	named env        aec-sandbox-env-<name>
//
// Retention: files live on the volume - they survive sessions, container restarts
// and server restarts. /tmp is a tmpfs, wiped whenever the container restarts.
// Disposing stops and removes the container but keeps the volume; data is deleted
// only through Sandbox.Dispose with removeData, Sandbox.RemoveAgentData or
// Provider.RemoveData - which the sandbox hub calls when an agent is deleted.
//
// Names are made injective: namesFor sends every character outside [A-Za-z0-9_.-] to
// "-", so Create and RemoveData refuse an id that would share a container and volume
// with another id (checkSandboxID).
//
// Container reuse: a container is reused when its aec.sandbox.config label equals the
// fingerprint of the settings (a sha256 of the image, network, resources, tmpfs and
// user, in a fixed byte layout), so a container created earlier with the same
// settings, by this or another process, is found and reused instead of recreated.
// The Node helper that runs every file operation inside the container is embedded
// byte for byte (embed/helper.js), checked against the recorded golden copy by a
// test and by wiregen --check in CI; embed/Dockerfile is the image's Dockerfile.
//
// Commands run through the helper's exec mode (one `docker exec` per command, see
// exec_stream.go): the host sends a header line and the helper answers AEC-OK or
// AEC-ERR before the command's output. A command that must stop (timeout, cancel,
// output cap) is stopped by closing the helper's stdin, which kills the command's
// process group inside the container; the docker client is killed only if it is still
// running five seconds later. If the container turns out to be down, Exec recovers it
// once and retries.
//
// The tests that need a Docker daemon are opt-in (AEC_DOCKER_TESTS=1) and use a name
// prefix of their own, aec-test-<hex>, so they never touch a container of the user.
//
// This is an opt-in leaf package: the root aec package does not import it, and
// with sandbox/host it is the only sandbox package allowed to import os and
// os/exec (threat T-05-08).
package docker
