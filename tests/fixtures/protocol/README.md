# Serving operational fixture binding

The normative protocol is `umir-operational-fixtures/1.1.0`. This test binding supplements accepted S4. Production handlers and public bootstrap constructors retain their interfaces. The general Auth discovery client separates host retry time from injected business time. The protocol specification and frozen C2 business wire files remain authoritative.

The test image runs `/fixture-server serve`. It reads only `/run/umir-fixtures/config/config.json`. The config must be a regular 0600 file owned by the process UID/GID. No environment or command option supplies credentials. `OwnerConfig` fields, strict JSON, fixed private port 8080 and no public port are required. Private URLs name only the declared project services. Serving does not use delivery metadata or durable state.

Supported operations are `fixture.ready`, `clock.read`, `clock.set` and `clock.checkpoint`. Other owner/sink operations fail `UnsupportedOperation`. Controls use authenticated HTTP over a 0600 Unix socket in the 0700 `/run/umir-fixtures` directory. The production HTTP handler contains no fixture route. The helper reads one bounded JSON request from stdin and returns only validated redacted protocol output. Credentials never appear in arguments or diagnostic output. Read/set/checkpoint responses require the server instant grammar before Go parsing: years 2000–2099, explicit offsets with hour 00–23 and minute 00–59, and optional dot fractions with one through nine digits.

Required credentialFiles keys and fixed file suffixes:

| Key | Actual owner mechanism |
| --- | --- |
| `serving-auth` | `ProcessHTTPOptions.ServingAuthKey`, outbound real Auth registry/JWKS |
| `auth-serving` | `ProcessHTTPOptions.AuthRuntimeKey`, inbound real Auth lifecycle |
| `gateway-serving` | `ProcessHTTPOptions.GatewayServingKey` |
| `serving-warehouse` | `ProcessHTTPOptions.ServingWarehouseKey` |

Every reference targets `serving` and `/run/umir-fixtures/secrets/<key>`. The separate controlCredentialFile targets `serving` and the suffix `fixture-serving`. Each file is a regular bounded 0600 file owned by the process UID/GID, admitted through a nonblocking no-follow descriptor before bounded reading. FIFOs fail without a writer. Keys must be distinct. The independently approved operator binding correction includes `serving-auth` for Serving and Auth. Factory correction and actual Auth provisioning remain pending integration prerequisites. Missing credentials fail startup. Do not reuse the lifecycle key or add fixture authentication to Auth.

One controlled clock supplies the actual registry, scoped handler/JWT verifier, real Auth key cache and memory runtimes through `bootstrap.NewProcessHTTP`. Host monotonic timers bound IO, registry polling and one-second stale/failed JWKS retry throttling. A general retry-clock dependency defaults to host `time.Now`; rollback restarts the throttle interval. Owner time still governs JWT and key-cache validity. They do not advance business time. Clock generations are decimal, start from config, and stop at the protocol's 19-digit limit. A compare-and-set increments once, including an equal instant. Replay returns the original response; changed payload conflicts. Checkpoint SHA256 hashes canonical JSON with sorted object keys for the returned redacted Clock object.

Each process creates fresh, separate instance and business epoch UUIDs. Readiness reads the existing authenticated runtime endpoint through the actual handler and requires its real ready flag and matching epoch. The endpoint remains the place to read business epoch; adding it to the strict fixture.ready envelope is forbidden. Embedded timezone data is identified as `go1.26.9-embedded`. Restarts use runner-provided checkpoint instant/generation and actual Auth reconciliation; memory is not reseeded.

Bounds: 16 KiB request, 256 KiB response, depth 16, eight concurrent controls, ten-second control/IO deadline, 256 cached IDs with no eviction. Healthchecks use fresh readiness observations and consume this finite cache. These fixtures are for bounded test runs; capacity exhaustion fails closed. No fault, job, scheduler, signer, bootstrap or durable-store replacement is supplied.

Build with Go 1.26.9 and GOWORK=off. Use `go build -trimpath -o <owned-output>/fixture-server ./tests/fixtures/server` and the analogous `fixture-control` command. Run `go test -race -count=1 ./tests/fixtures/...`, complete `make check`, `make quality-test`, `go mod verify`, Linux amd64/arm64 builds and the fixture Docker build. Transport doubles in composition tests establish only wrapper behavior. A Docker smoke with no Auth checks fail-closed readiness and private controls; it cannot establish J1/E acceptance. Fresh independent review and real Factory/Auth/Warehouse bindings are separate gates.
