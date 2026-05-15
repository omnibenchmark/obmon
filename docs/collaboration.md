# Collaboration

`obmon` lets you hand off a recorded run to a coworker without standing up
shared infrastructure. Transfer happens peer-to-peer over
[croc](https://github.com/schollz/croc): the sender announces a short code on a
public relay, the receiver redeems it. No account, no upload step, no expiry
beyond "the sender's process is still running."

## Sending a run

Every `obmon stream` invocation records the streamed telemetry into a local cache under a run ID. List them:

```sh
obmon runs list
```

Pick the run you want to share and send it:

```sh
obmon share <run-id>
```

`obmon` prints a transfer code and blocks until a receiver connects:

```
share code: 1234-banana-orbit-cobra
receive with: obmon receive 1234-banana-orbit-cobra
waiting for receiver...
```

Send that code to your coworker via whatever channel you like. The code itself is enough to authorize the transfer, so treat it like a one-time password.

## Receiving a run

On the other machine:

```sh
obmon receive 1234-banana-orbit-cobra
```

The telemetry file is downloaded into the local cache under the original run ID. If that run already exists locally, `obmon` refuses to overwrite it and points you at `obmon replay` instead.

## Replaying

A received run is just another entry in the cache, so replay it into the local Aspire dashboard with:

```sh
obmon replay <run-id>
```

This starts Aspire if it's not running and feeds the cached telemetry to it. The receiver sees exactly what the sender saw — same logs, same traces, same run ID.

## Notes

- Transfers go through croc's default public relay. No data is stored on the relay; it only brokers the peer connection.
- Both sides must be online at the same time. There is no "leave it on the relay overnight" mode.
- Only the telemetry file is shared. Source benchmarks, configs, and outputs are not.
