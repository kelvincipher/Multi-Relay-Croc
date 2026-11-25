# Multi-Relay Croc

Multi-Relay Croc is a Go extension of [croc](https://github.com/schollz/croc) that distributes an encrypted file transfer across two independent relay server instances. Its goal is to limit how much of a transfer's ciphertext one compromised relay can observe while retaining Croc's peer authentication and end-to-end encryption.

The project adds concurrent two-relay transfer, an ordered pool of backup relay pairs, and automatic reconnection with transfer resumption. It is a university project intended for reproducible experimentation with relay separation and recovery. The implementation, configuration examples, and supporting documentation are in [`source/`](source/).

## How it works

Each transfer attempt uses exactly two file-data relays. Files are read in 32 KiB chunks, with even-indexed chunks assigned to the first relay and odd-indexed chunks to the second. Both lanes send concurrently, and the receiver reconstructs the file regardless of arrival order. The data path reuses Croc's AES-GCM encryption and PAKE-based peer authentication. File identity is carried inside encrypted data frames, and the receiver checks the completed file against its SHA-256 hash before reporting completion. The implementation also supports multiple regular files, per-file compression, and empty files.

Both clients load the same local YAML configuration, which defines relay pairs in priority order. After authenticating, they exchange a configuration fingerprint and confirm the transfer identity, active pair, attempt number, and peer roles before sending file payloads. Control messages include authenticated direction and sequence information to detect replay. Configuration validation rejects reused relay identities, overlapping endpoints, and recognizable address aliases. Relay credentials can be supplied through environment variables.

If either relay or the control connection fails, the clients close the entire active pair and move to the next unused pair. They establish a fresh authenticated session rather than replacing only the failed lane. The receiver retains records of chunks that passed authentication and were written in full, checks those chunks again on disk, and requests only the missing or invalid ranges. Chunk assignments remain tied to file offsets, so retrying does not change their parity. Every pair in the configured pool must be disjoint from the others.

Once Multi-Relay mode is selected, the client does not silently fall back to a single relay, a local or direct peer-to-peer data connection, or stored transfer. If it cannot complete a safe attempt and no unused pair remains, it returns an error. The protocol and recovery flow are described in the [architecture guide](source/ARCHITECTURE.md).

## Getting started

Building from source requires Go **1.27.3** or newer, as specified in [`go.mod`](source/go.mod). The local demonstration also requires Python 3. From a terminal with those dependencies installed, clone the repository and run the demonstration from the source directory:

```sh
git clone https://github.com/kelvincipher/Multi-Relay-Croc.git
cd Multi-Relay-Croc/source
go build -o croc-multirelay .
python3 scripts/multirelay/demo.py --binary ./croc-multirelay
```

The demonstration starts four relay processes, prepares matching client configurations, runs a sender and receiver, verifies their file hashes, and cleans up the processes afterward. Adding `--kill a` or `--kill b` to the Python command terminates one primary relay during transfer to exercise recovery through the backup pair. See the [demo script](source/scripts/multirelay/demo.py) for its implementation.

For regular use, both endpoints need this extension and a matching relay configuration. Multi-Relay mode activates when a valid `multirelay.yaml` is installed in Croc's configuration directory, or when a file is selected with `CROC_MULTIRELAY_CONFIG` or `--multirelay-config`. Without a selected or installed configuration, the client retains ordinary Croc behavior. A missing explicitly selected file or an invalid configuration produces an error.

Sending retains the familiar `croc send <file>` workflow once the built executable is installed under that name. Receiving follows the upstream platform policy: Windows accepts a transfer code as an argument, while Unix defaults to an interactive prompt or `CROC_SECRET`. The [setup guide](source/SETUP_GUIDE.md) covers client commands, configuration locations, relay passwords, manual deployment, and the optional [Docker Compose configuration](source/deploy/multirelay/compose.yaml). A [sample relay pool](source/examples/multirelay/relays.yaml) is included.

## Security model

The intended attacker controls one relay instance and may observe, record, modify, replay, delay, or drop its traffic. Splitting encrypted payload chunks across independent lanes limits that relay's view of the logical transfer. This separation supplements the existing encryption. It does not make AES-GCM cryptographically stronger or introduce secret sharing or erasure coding.

The design does not protect against compromised endpoints, simultaneous compromise of multiple relays, or a provider-wide compromise that reaches both instances. It also does not provide anonymity, hide traffic timing or volume, or prevent denial of service. A malicious relay can interrupt a transfer, and exhausting the backup pool remains a valid failure outcome.

Relay independence is a deployment responsibility. Distinct identifiers and addresses cannot prove that two endpoints run on separate, isolated machines. The four-process localhost demonstration verifies functionality but does not establish geographic separation or resistance to compromise of the host. Real deployments need isolation appropriate to the threat model, even when the instances share a hosting provider.

## Current limitations

Nonempty files must be larger than **32 KiB (32,768 bytes)**. Smaller files would occupy only one payload chunk, exposing their entire encrypted payload to one relay. The current implementation rejects them before transmission because ciphertext fragmentation for that case has not been implemented. Empty files are handled through metadata. Symbolic links are not supported in Multi-Relay mode, and the main chunk size is fixed.

Resumption works within the same running client processes. The receiver's verified-chunk records are held in memory and are not recovered after a client restart. Memory use grows with the number of tracked chunks, and very large transfers have not been benchmarked. An interrupted transfer may leave a partial destination file, which must not be treated as a completed result.

Recovery is bounded by the configured pool and connection watchdogs. The current defaults use a 120-second setup timeout and a 45-second idle timeout during transfer. Slow or asymmetric connections can exhaust the pool, and successful recovery is not guaranteed for every network failure pattern. Local-only operation, explicit peer IP routing, single-stream mode, QR-based reception, and strict `derp` transport are incompatible with Multi-Relay mode.

## Testing and project status

The repository includes unit and integration tests for configuration matching, chunk routing and reconstruction, relay exposure, failover, preserved progress, integrity verification, and incompatible peers. Fault-injection tests exercise corrupted, replayed, dropped, and delayed traffic. The local CLI demonstration complements these tests by running independent relay and client processes.

The [testing guide](source/TESTING.md) records the reported test results, race-detector commands, regression coverage, and known baseline failures. It does not claim that the entire upstream test suite passes. The included [build record](BUILD_RESULTS.json) reports successful Linux demonstrations and a Windows cross-build. Windows execution and Docker Compose execution remain unverified, as do geographically separated deployment, long-running stress tests, and an independent security audit.

## Documentation and upstream

Start with the [setup guide](source/SETUP_GUIDE.md) to run the project, the [architecture guide](source/ARCHITECTURE.md) to understand the protocol, and the [testing guide](source/TESTING.md) to reproduce its checks. These supporting guides are currently written in Vietnamese.

This project builds on [schollz/croc](https://github.com/schollz/croc), using upstream commit [`d77d59a`](https://github.com/schollz/croc/commit/d77d59a8931d67460574f7603b9446470d475070) as its implementation baseline. The Go module retains the upstream path `github.com/schollz/croc/v11`. The source is distributed under the [MIT license](source/LICENSE), with upstream copyright and third-party notices retained.
