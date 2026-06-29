# Bringing E2B Sandbox Back to Local Development: Why I Built e2b-local

Let me start with what this project actually is: [`e2b-local`](https://github.com/superduck-ai/e2b-local) is a local, E2B-compatible API gateway. Your app still talks to the E2B SDK. You point the SDK API URL at your machine, and calls like `Sandbox.create()`, `commands.run()`, filesystem access, and PTY keep the same shape. The difference is what happens behind the gateway: instead of creating a sandbox in the cloud, it starts one locally, either as a Docker container or as an OrbStack Linux VM.

![e2b-local overview](https://store-tg1.cvte.com/3d26f13d-22d8-4ffb-8e0b-7e512984dae3.png)

I think of it as an adapter between the E2B control plane and local runtimes. On the top, it tries to preserve the SDK experience. Underneath, it translates templates, volumes, envd, and sandbox lifecycle operations into Docker or OrbStack operations. In Docker mode, a local Docker image becomes the template. In OrbStack mode, an existing machine becomes the template, and a new sandbox VM is created by cloning it.

I did not start this because I wanted to rebuild E2B, or because I thought cloud sandboxes were bad. The thing that pushed me into it was much more mundane: the feedback loop for developing templates was too long.

When a template is still moving, changes are small and frequent. Maybe I changed one system dependency, tweaked the start command, fixed a ready check, or added a temporary debug tool. In the normal cloud workflow, every change goes through build, upload, publish, create sandbox, then verify. One pass is fine. Several passes in a row start to break concentration.

The other problem is local networking. A sandbox often needs to call services that only exist on my laptop: a local API server, a mock service, a temporary database, or an internal service that has not been deployed yet. You can always build tunnels or workarounds for remote sandboxes, but at that point I am no longer just testing whether the template works.

So the goal of this project is narrow on purpose: keep the E2B SDK workflow, but run the actual sandbox on my machine. It is not a replacement for E2B Cloud. It is a shorter local loop for template development.

## First, Split the Boundary

At first, it is tempting to describe this as "start a local container and connect the SDK to it." But once I looked at the E2B call path, there were really two different layers.

The first is the control plane: create a sandbox, delete it, list templates, manage volumes, fetch logs and metrics. These requests fit naturally in a local gateway, which can translate them into runtime operations.

The second is the data plane: run commands, read and write files, open a PTY, handle streaming. Those operations are served by `envd` inside the sandbox. The gateway should not pretend to own all of that traffic. Its job is to create the sandbox and return that sandbox's `envdURL`. After that, the SDK can talk to envd directly for commands, filesystem, and PTY.

That split shaped the whole project. `e2b-local` does not need to reimplement the envd protocol, and it does not need to proxy every byte of sandbox data through the gateway.

The HTTP layer is written with Gin. E2B request and response DTOs are generated from the OpenAPI schema with `oapi-codegen` into `internal/e2bapi`. I did not want to handwrite structs that merely looked close enough, because SDK compatibility is the main contract here. Small differences in field names, enums, or response shapes tend to become confusing SDK bugs later.

The SDK side stays light:

```bash
export E2B_API_URL="http://127.0.0.1:3000"
export E2B_API_KEY="local"
unset E2B_SANDBOX_URL
```

`E2B_API_KEY` is mostly there to satisfy the SDK's usual shape. The local gateway does not depend on a real cloud E2B key.

## Docker Was the Shortest First Step

For template debugging, Docker is the obvious first runtime. E2B custom templates are already closely tied to Docker images. The cloud runtime is based on Firecracker microVMs, but from the user's side, building a template usually starts with a Dockerfile and a `linux/amd64` image.

So the Docker backend is simple in spirit: local Docker images become templates. You build and tag an image locally, `e2b-local` creates a sandbox from it, and once the behavior looks right you can move the same Dockerfile or image-building logic into the cloud E2B template flow.

For example:

```bash
docker buildx build \
  --platform linux/amd64 \
  -t e2b-local/code-interpreter:latest \
  --load .
```

Then the SDK can create `code-interpreter`, and the local gateway resolves that template ID back to the local Docker image.

One deliberate choice here: the Docker backend does not call `docker run` or `docker ps`. It talks directly to the Docker Engine API. The reason is the same reason that later pushed me away from the OrbStack CLI: the gateway is a long-running service. It should hold a structured client, not keep forking CLI processes and parsing stdout or stderr.

Docker host resolution follows the local development environment: first respect `DOCKER_HOST`; if the current user has an OrbStack Docker socket, use `~/.orbstack/run/docker.sock`; otherwise fall back to the common `unix:///var/run/docker.sock`. Image inspect, container create/start/stop/remove, logs, stats, and volume operations all go through the Docker SDK client.

Template mapping also avoids introducing a separate registry. Local tagged images are template candidates. Dangling images are ignored. Snapshot images labeled with `e2b.local.snapshot=true` are kept out of the template list. By default, the template ID comes from the last component of the image reference:

```text
e2b-local/code-interpreter:latest  -> code-interpreter
python:3.11                        -> python
ghcr.io/acme/my-template:v1        -> my-template
```

If an image was built through the `e2b-local` template build API, the gateway writes labels such as `e2b.local.template_id`, template names, build ID, start command, and ready command. Those labels override the default inference later. Simple images work with almost no setup, while richer templates can still carry explicit metadata.

When a Docker sandbox starts, `e2b-local` does not modify the user's image or rebuild a new layer. It picks the bundled envd binary based on the image architecture:

```text
linux/amd64 -> envd-bin/envd-linux-amd64
linux/arm64 -> envd-bin/envd-linux-arm64
```

Then it bind-mounts envd into the container as read-only:

```text
host:      envd-bin/envd-linux-amd64
container: /usr/local/bin/envd
readonly:  true
```

The container entrypoint is replaced with `/usr/local/bin/envd`. envd listens on `49983/tcp` inside the container. Docker assigns a random host port, bound to `127.0.0.1`. After the container starts, the gateway inspects the port mapping and returns something like `http://127.0.0.1:<random-port>` as the sandbox `envdURL`.

If the template labels contain a start command, the gateway passes it to envd through `-cmd`. If there is a ready command, the gateway first waits for envd health, then runs the ready command. If anything fails, the new container is removed and the error tries to include container logs. That matters a lot locally, because "creation failed, no idea why" is the worst kind of development loop.

## OrbStack Is a Different Shape of Local Sandbox

Docker is great for lightweight environments, but not every template wants to be a container. Some things behave more like a full Linux machine: they need systemd, a more host-like process model, or a base machine that you can tune over time.

That is where the OrbStack backend fits.

On macOS, OrbStack VMs start quickly, the filesystem experience is good, and OrbStack already provides both Docker compatibility and Linux machines. For `e2b-local`, an existing OrbStack machine can naturally become a template. Creating a sandbox means cloning that template machine, starting the cloned VM, copying envd into it, installing a systemd service, and waiting for envd to come up.

At the very beginning, I did consider just shelling out to `orb`. The commands are obvious and good enough for a prototype:

```text
orb clone <template-vm> <sandbox-vm>
orb start <sandbox-vm>
orb stop <sandbox-vm>
orb delete --force <sandbox-vm>
orb info --format json <vm>
orb list --format json
orb config set machine.<vm>.isolated true
orb config add machine.<vm>.mounts <host-path>:<vm-path>
```

But as soon as I pushed the prototype further, this no longer looked like the interface I wanted to keep.

The CLI is great for humans, but it is not ideal as the internal control protocol of a gateway. Every lifecycle operation forks an `orb` process. Process startup is only part of the cost. Timeout handling, cancellation, stderr parsing, and error classification all become awkward. Even when `orb info --format json` returns structured data, error paths often come back as human-readable text. That text eventually becomes an SDK error, and the result is not very clean.

There is also a deeper reason. OrbStack VM initialization is not just clone/start. `e2b-local` has to write envd into `/usr/local/bin`, create a systemd unit, write sandbox metadata, create volume symlinks, then run `systemctl daemon-reload && systemctl restart envd`. If everything goes through `orb run` or `orb push`, the implementation becomes tied to a larger CLI semantics instead of being cleanly split between "control the VM lifecycle" and "enter the VM to write files and manage systemd."

So the question changed: instead of treating `orb` as the only interface, I wanted to see how `orb` itself talks to the OrbStack daemon. The CLI ultimately has to communicate with a local daemon. If I could connect to the Unix domain socket behind it, I could remove one layer.

## How I Confirmed the OrbStack UDS Protocol

This was not guesswork. I used two sources of evidence: reverse engineering the Go client, and capturing real socket traffic.

On macOS, `orb`/`orbctl` is a Go program. For Go binaries, `go version -m` and `strings` are surprisingly useful. They can expose module paths, dependencies, and method names that were not fully stripped. A few names stood out:

```text
github.com/creachadair/jrpc2
ContainerStart
ContainerStop
ContainerDelete
ContainerClone
ContainerSetConfig
ListContainers
MachineConfig
MachineMount
sconrpc.sock
sconssh.sock
```

That was enough to point in the right direction: the control plane looked like JSON-RPC, VM lifecycle operations mapped to a group of `Container*` methods, and `sconssh.sock` looked like the path for entering machines.

`ListContainers` is a good concrete example of what "talking directly to the OrbStack daemon" means. This is not a new HTTP server listening on a TCP port. It is JSON-RPC over a Unix domain socket. You can try it with `curl`:

```bash
curl --unix-socket "$HOME/.orbstack/run/sconrpc.sock" \
  -H 'Content-Type: application/json' \
  -X POST http://sconrpc \
  --data '{"jsonrpc":"2.0","id":1,"method":"ListContainers"}'
```

`http://sconrpc` is not a real network address. `curl --unix-socket` still needs a URL to build the HTTP request, so the host is just a placeholder. The actual connection goes through `$HOME/.orbstack/run/sconrpc.sock`.

The response looks roughly like this. IDs, IPs, and disk sizes will differ on a real machine:

```json
{
  "jsonrpc": "2.0",
  "id": 1,
  "result": [
    {
      "record": {
        "id": "01GQQVF6C60000000000DOCKER",
        "name": "docker",
        "image": {
          "distro": "docker",
          "version": "latest",
          "arch": "arm64",
          "variant": "default"
        },
        "config": {
          "isolated": false,
          "forward_ssh_agent": true,
          "isolate_network": false,
          "default_username": "root",
          "http_port": 0,
          "https_port": 0
        },
        "builtin": true,
        "state": "running"
      },
      "disk_size": 2634473472,
      "ip4": "192.168.139.2",
      "ip6": "fd07:b51a:cc66::2"
    },
    {
      "record": {
        "id": "01KTK0Z32XA8Y4R8MVY2F4TZKN",
        "name": "ubuntu-2404",
        "image": {
          "distro": "ubuntu",
          "version": "noble",
          "arch": "arm64",
          "variant": "cloud"
        },
        "config": {
          "isolated": false,
          "forward_ssh_agent": true,
          "isolate_network": false,
          "default_username": "arthur",
          "http_port": 0,
          "https_port": 0
        },
        "builtin": false,
        "state": "running"
      },
      "disk_size": 1155293184,
      "ip4": "192.168.139.198",
      "ip6": "fd07:b51a:cc66:0:18cb:1bff:fe4a:2ea0"
    }
  ]
}
```

This is immediately more useful to a program than the human-facing output of `orb list`. `record.name` is the machine name. `record.image` tells us the distro and architecture. `record.state` tells us whether it is running or stopped. `record.config.isolated` maps to the isolation setting we need later. `builtin: true` identifies the built-in `docker` machine so it does not accidentally become a user template.

For `e2b-local`, this one method covers most of the list/info entry point: list machines, filter usable templates, check whether a sandbox VM already exists, and read its state and network address. Clone/start/stop/delete/config follow the same JSON-RPC channel with different methods. This is the moment where `orb list --format json` stopped needing to exist inside the gateway.

Strings alone were not enough, because I still needed to confirm payload shape, socket paths, and which methods the CLI used in real flows. So I used `socat` as a Unix socket middleman. The idea is to rename the real socket, put a new listener at the original path, forward traffic to the real socket, and dump both directions.

First, find the socket. Current versions commonly place it under `~/.orbstack/run`:

```bash
ls -la ~/.orbstack/run
ls -la ~/.orbstack/run/vmcontrol.sock
ls -la ~/.orbstack/run/sconrpc.sock
```

If your version differs, search a wider area:

```bash
find ~/.orbstack /Applications/OrbStack.app \
  \( -name "vmcontrol.sock" -o -name "sconrpc.sock" \) 2>/dev/null
```

Install `socat`:

```bash
brew install socat
```

Then use `vmcontrol.sock` as the proxy target:

```bash
SOCK="$HOME/.orbstack/run/vmcontrol.sock"

mv "$SOCK" "$SOCK.real"

socat -v \
  "UNIX-LISTEN:$SOCK,fork" \
  "UNIX-CONNECT:$SOCK.real" \
  2>&1 | tee /tmp/vmcontrol-dump.log
```

Run `orb list`, `orb info`, or operate on a machine in the OrbStack UI. For plaintext protocols like JSON-RPC, the dump usually shows method, params, and id directly. When you are done, restore the socket path:

```bash
rm -f "$SOCK"
mv "$SOCK.real" "$SOCK"
```

Some older versions or special installs may place the socket inside the app bundle. The same idea works there, but requires `sudo`:

```bash
sudo mv /Applications/OrbStack.app/Contents/MacOS/vmcontrol.sock \
        /Applications/OrbStack.app/Contents/MacOS/vmcontrol.sock.real

sudo socat -v \
  UNIX-LISTEN:/Applications/OrbStack.app/Contents/MacOS/vmcontrol.sock,fork \
  UNIX-CONNECT:/Applications/OrbStack.app/Contents/MacOS/vmcontrol.sock.real \
  2>&1 | tee /tmp/vmcontrol-dump.log
```

The goal was not to own every internal OrbStack protocol. It was to confirm the boundary this project actually needed: list/info/clone/start/stop/delete/config can go through JSON-RPC on `sconrpc.sock`; writing files inside the VM and installing systemd services can go through SSH on `sconssh.sock`. `orb run --machine <vm> /bin/sh -lc <script>` turned out not to be required, because VM-side work can be done through SSH directly.

The resulting code is intentionally thin. `internal/orbctl` sends JSON-RPC HTTP requests over the Unix socket and wraps `ListContainers`, `ContainerClone`, `ContainerStart`, `ContainerStop`, `ContainerDelete`, and `ContainerSetConfig`. When the OrbStack backend needs to write root-owned files or run systemd commands, it uses a Go SSH client connected to `~/.orbstack/run/sconssh.sock`. No system `ssh`, no `orb` process.

At that point the two backends finally had the same shape: Docker does not shell out to `docker`, and OrbStack does not shell out to `orb`. The gateway talks directly to local daemon/socket interfaces, receives structured responses, and controls its own timeouts and errors.

## A Small Volume Metadata Problem

OrbStack volume support has one small but representative design choice: how should local directories be named, and where should their metadata live?

The Docker backend can use Docker native named volumes. For OrbStack VMs, volumes are better represented as local directories, by default under:

```text
~/.e2b-local/volumes
```

When a sandbox is created, the backend uses OrbStack selective mounts to mount the directory into the VM, then creates a symlink inside the VM to the path requested by the SDK. If `orbstack.isolated: true` is enabled, the sandbox VM does not see the whole macOS filesystem, only the explicitly mounted volumes.

The simplest directory name would be the volume ID:

```text
~/.e2b-local/volumes/vol_01HX...
```

That makes lookup easy, but it is terrible for humans browsing the directory. Using the user-provided volume name is much nicer:

```text
~/.e2b-local/volumes/data
~/.e2b-local/volumes/cache
```

The problem is that name is not a stable primary key. It can collide, and it might need to change later. In the E2B API, the stable identity is the volume ID.

So the design splits the two concepts. The directory name stays readable, such as `data`, `cache`, or `data-2`. The stable identity is written into an extended attribute on the directory. The xattr key is:

```text
com.e2b.local.volume-meta
```

The value is a tiny JSON object:

```json
{"VolumeID":"vol-123","Name":"data"}
```

I chose xattr instead of writing `.e2b-meta.json` inside the volume directory because that directory is mounted into the VM and should contain user data. Extra metadata files are easy to see, edit, or delete accidentally. xattr feels more like host-side metadata attached to the directory, and it does not show up as part of the sandbox data plane.

There is also a migration path. Early versions may have left behind the old xattr key `com.e2b.volume-meta` or an old `.e2b-meta.json` file. Metadata loading tries the new xattr, then the old xattr, then the old file. If it reads an old format, it writes back the new `com.e2b.local.volume-meta` and cleans up the old key or file. Historical `Token` fields are dropped during re-encoding; current volume metadata only keeps `VolumeID` and `Name`.

This is not an OrbStack-required format. It is how `e2b-local` manages local volume directories in the macOS + OrbStack setup: stable API identity, readable folders, and no control-plane metadata leaking into sandbox data.

## envd Cannot Depend on My Machine

One issue had to be fixed before the project could be shared: the envd binary path.

During early prototyping, it is very easy to point at an absolute path on your own machine. That works for one developer, then fails immediately for everyone else.

The repository now carries Linux envd binaries directly:

```text
envd-bin/envd-linux-amd64
envd-bin/envd-linux-arm64
```

These are not a reimplementation of envd. They are built from E2B's source, so SDK data-plane behavior for commands, filesystem, PTY, and streaming can stay close to real E2B sandboxes.

The Docker backend bind-mounts the right envd binary into `/usr/local/bin/envd` inside the container. The OrbStack backend copies envd into the VM and installs it as a systemd service. Config can still use relative paths, but they are resolved relative to the config file location so the project does not depend on a private path from my laptop.

## SDK Callers Should Barely Notice

After all that runtime work, the SDK user should see something boring.

TypeScript still looks like this:

```ts
import { Sandbox } from 'e2b'

const sandbox = await Sandbox.create('code-interpreter')
const result = await sandbox.commands.run('echo "hello from e2b-local"')
console.log(result.stdout)
await sandbox.kill()
```

Go can use [superduck-ai/e2b-go-sdk](https://github.com/superduck-ai/e2b-go-sdk):

```go
sandbox, err := e2b.Create(ctx, "code-interpreter", nil)
if err != nil {
	panic(err)
}
defer sandbox.Kill(ctx, nil)

result, err := sandbox.Commands.Run(ctx, `echo "hello from e2b-local"`, nil)
if err != nil {
	panic(err)
}
fmt.Println(result.(*e2b.CommandResult).Stdout)
```

The main differences are where templates and volumes come from. Docker templates are local Docker images. OrbStack templates are existing OrbStack machines. Docker volumes are Docker native named volumes. OrbStack volumes are local directories under `orbstack.volume_host_path`. The gateway and backend handle those differences; the SDK keeps the E2B style.

## Where This Fits

`e2b-local` is best for local development and template debugging. It is useful when you want to quickly check whether a Docker image can behave like an E2B template, tune start commands and ready checks, test system dependencies, let a sandbox call local development services, or use an OrbStack VM to approximate a fuller Linux host on macOS.

It is not trying to replace E2B Cloud's production isolation or turn a laptop into a production sandbox platform. It is a development adapter: shorten the feedback loop, surface template and runtime problems earlier, and keep the SDK code mostly unchanged.

Looking back, the important decisions all came from the same place: the gateway should talk to structured runtime interfaces instead of treating CLIs as long-term dependencies. Docker uses the Engine API. OrbStack uses JSON-RPC and SSH over Unix sockets. envd keeps owning the data plane. The result is lighter, easier to reason about, and closer to what a long-running local gateway should be.

That is why I stopped relying on `orb` commands and spent time analyzing OrbStack's socket communication. It was not about being clever or going lower-level for its own sake. For a gateway like `e2b-local`, removing one process fork and one layer of human-oriented output parsing makes a lot of later problems simpler.

## What I Have Not Implemented Yet

The current implementation focuses on the local development path: create a sandbox, start envd, run commands/filesystem/PTY, manage templates and volumes, and validate environments quickly with Docker or OrbStack. In other words, I first wanted to make "can this template run locally?" feel good before trying to cover every E2B Cloud API semantics.

The most obvious incomplete area is snapshot support. In E2B Cloud, a snapshot is not just "save the current environment." It touches template lifecycle, namespaces, permissions, long-term storage, creating future sandboxes from snapshots, and the build system. `e2b-local` is currently focused on the local debugging loop, so even if some runtimes can provide a thin local snapshot mapping, I do not want to present that as full cloud-compatible snapshot semantics.

The same applies to platform-side features: richer metrics/logs semantics, network policy, access token and API key management, teams and permissions, quotas, auditing, cross-machine scheduling, and long-term resource governance. These are important in a cloud platform, but they are not the first priority for local template development. `e2b-local` is better understood as a development tool, not a production control plane.

## Summary

`e2b-local` is a small idea with a useful payoff: keep the E2B SDK experience, but move the slowest part of template development back to the local machine. Docker handles lightweight image debugging. OrbStack VM handles cases that need something closer to a full Linux host. Avoiding `docker`/`orb` CLI calls and talking directly to structured local runtime interfaces is what keeps the gateway lighter and more reliable.

Star it here: https://github.com/superduck-ai/e2b-local
