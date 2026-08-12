# SBX Lifecycle Measurements

Date: 2026-08-12 CST

## Why the Previous Table Was Removed

The previous cross-backend table mixed two different measurements:

- Raw runtime resource creation (`create` and `start`).
- e2b-local Gateway completion (`CreateSandbox` returns after envd health and,
  for SBX, the reverse tunnel are ready).

Those are not interchangeable. In particular, Gateway readiness includes work
outside the Docker Engine lifecycle, so ranking it next to a raw container
create/start result does not establish that SBX itself is slower. The old table
also included an Apple Container fallback that added a failed three-second
health probe. It has been removed instead of being treated as a backend
comparison.

## Reproducible Scope

The SBX base is rebuilt from the current `e2b-local` source and imported into
the authenticated `sandboxd` Docker endpoint before each measurement:

```sh
scripts/build-sbx-image.sh
```

The build uses `--pull --no-cache`, compiles only `sbx-init` and `sbx-tunnel`
from this repository, and uses the checked-in, architecture-matched `envd`
artifact. It does not clone or compile an upstream repository. The BuildKit critical path
for this no-cache run was about `55s` (dependency download
`41.6s`, helper compilation `11.9s`, image export `0.9s`); the script prints
its elapsed time after every completed run.

For the raw Docker Engine comparison, both endpoints use the same freshly
built `e2b-local/sbx-envd:dev` image (local and SBX image ID
`sha256:ffe15170fb67...`). One warm-up run per endpoint is excluded, then each
recorded run performs a new container `create -> start -> forced remove` with
the image entrypoint overridden to `sleep 30`:

```sh
go run scripts/benchmark-sbx-lifecycle.go \
  -runs 5 \
  -local-docker-host "$(docker context inspect --format '{{(index .Endpoints "docker").Host}}')" \
  -sbx-docker-host "unix://${HOME}/.sbx/run/d/docker.sock"
```

This deliberately does not benchmark envd bootstrap, the Gateway HTTP layer,
or the SBX reverse tunnel.

## Raw Engine Results

Host: macOS arm64. Local Docker endpoint: OrbStack. SBX: authenticated
`sandboxd` Docker endpoint. Values are milliseconds.

| Target | Runs | p50 | Mean |
| --- | --- | ---: | ---: |
| Local Docker | 166, 146, 200, 127, 121 | 146 | 152 |
| SBX Docker | 299, 300, 345, 328, 279 | 300 | 310 |

On this machine and image, the SBX raw Docker path is about `154ms` slower at
p50. It is not evidence that an SBX implementation is generally faster or
slower than a different runtime; the comparison only states the measured path.

## Gateway Readiness

`CreateSandbox` for the SBX runtime additionally creates the sandboxd resource,
starts envd, establishes the tunnel, and waits for `/health`. That is a separate
end-to-end service-level measurement and must be compared only against another
runtime executing the same e2b-local bootstrap contract. The old document did
not meet that condition, so it no longer reports a Gateway readiness ranking.

The SBX integration suite exercises that path and its supported lifecycle
operations; it is run separately with:

```sh
go test -tags=sbx_runtime_integration ./internal/backends/sbx -count=1
```
