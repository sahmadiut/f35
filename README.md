# F35

F35 is an end-to-end DNS resolver scanner for real tunnel testing.

It does not only test DNS replies.
It actually:

1. starts a tunnel client
2. sends traffic through the tunnel
3. prints only resolvers that really work

This is useful when you need to find DNS resolvers that still pass traffic during filtering or shutdown conditions.

## What You Need

You need:

- a resolver list file
- a reachable tunnel domain
- a tunnel client:
  - `dnstt-client`
  - `slipstream-client`
  - `vaydns-client`
- the correct client flags for your tunnel in `-a`

## Build

```bash
CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o f35 .
```

## Resolver List

One resolver per line:

```txt
1.1.1.1
8.8.8.8:53
9.9.9.9
```

If you write only an IP, F35 uses port `53`.

## Basic Usage

```bash
f35 -r resolvers.txt -e ENGINE -d DOMAIN [other flags]
```

Important flags:

- `-r`: resolver list file
- `-e`: engine: `dnstt`, `slipstream`, or `vaydns`
- `-d`: tunnel domain
- `-a`: extra client flags for the selected engine
- `-x`: proxy type used for the HTTP test: `http`, `https`, `socks5`, `socks5h`
- `-w`: concurrent workers
- `-s`: wait time before testing, in milliseconds
- `-t`: HTTP timeout, in seconds
- `-R`: retries per resolver
- `-p`: full path to the client binary if it is not in `PATH`

## How `-a` Works

Use `-a` to pass the tunnel client flags you already use in real life.

Examples:

- `dnstt`: `-a '-pubkey YOUR_PUBLIC_KEY'`
- `vaydns`: `-a '-pubkey YOUR_PUBLIC_KEY -log-level info -udp-timeout 200ms'`

F35 inserts resolver, domain, and local listen address automatically.

For `dnstt`, `-a` is placed before the positional `domain` and `listen` arguments.

## Examples

DNSTT:

```bash
f35 -r resolvers.txt \
  -e dnstt \
  -d t.example.com \
  -x socks5h \
  -a '-pubkey YOUR_PUBLIC_KEY'
```

VayDNS:

```bash
f35 -r resolvers.txt \
  -e vaydns \
  -d t.example.com \
  -x socks5h \
  -a '-pubkey YOUR_PUBLIC_KEY -log-level info -udp-timeout 200ms -open-stream-timeout 7s -idle-timeout 10s -keepalive 2s -udp-workers 200 -rps 300 -max-streams 0 -max-qname-len 101 -max-num-labels 2'
```

Slipstream:

```bash
f35 -r resolvers.txt \
  -e slipstream \
  -d t.example.com \
  -x socks5h
```

Save healthy resolvers:

```bash
f35 -r resolvers.txt -e dnstt -d t.example.com -x socks5h -a '-pubkey YOUR_PUBLIC_KEY' | tee healthy.txt
```

Use a binary outside `PATH`:

```bash
f35 -r resolvers.txt -e vaydns -d t.example.com -x socks5h -p ./vaydns-client -a '-pubkey YOUR_PUBLIC_KEY'
```

More stable scan:

```bash
f35 -r resolvers.txt -e vaydns -d t.example.com -x socks5h -w 50 -s 2000 -t 8 -R 2 -a '-pubkey YOUR_PUBLIC_KEY'
```

## Output

Healthy resolvers are printed to stdout:

```txt
1.2.3.4:53 342ms
5.6.7.8:53 89ms
```

If a resolver fails, F35 prints nothing for it.

## Practical Tips

- If results are empty, increase `-s` first.
- If results are unstable, lower `-w`.
- If very slow resolvers still work, increase `-t`.
- If your tunnel needs auth, key, or tuning flags, put them in `-a`.
- If your client binary is missing, use `-p`.

## Troubleshooting

`binary ... not found in PATH`

- install the client
- add it to `PATH`
- or use `-p /full/path/to/client`

No output

- check the domain
- check the engine with `-e`
- check your tunnel flags in `-a`
- try `-s 2000`
- try `-t 8`
- try `-R 1`

Very few working resolvers

- lower `-w`
- increase `-s`
- increase `-t`
- add retries with `-R`
