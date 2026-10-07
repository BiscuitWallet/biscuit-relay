# biscuit-relay

The server behind [biscuitwallet.com](https://biscuitwallet.com): one small Go program
that serves the website and its onion service, the public price data the app reads,
and the relay for exchange swaps through [Trocador](https://trocador.app).

It is published so that anyone can read what our server does with the requests of
[Biscuit Wallet](https://github.com/BiscuitWallet/Biscuit). What it records, why and for
how long is also explained in the [privacy policy](https://biscuitwallet.com/privacy/).
A server can't be inspected from the outside: this is the code we run, built as
described below.

## The exchange swap relay

Biscuit calls the relay; the relay adds our Trocador partner key and passes the request
on. The key never ships inside the app.

- **Allow list**: only `coins`, `new_rate`, `new_trade`, `trade` and `validateaddress`
  go through, with GET. Everything else gets a 404.
- **Nothing about the user goes to Trocador**: Trocador receives the key and a fixed
  user agent (`biscuit-relay`). No IP address, cookies or headers from the app.
- **Swap log**: when a `new_trade` succeeds, and only then, one line is written: time,
  IP address, user agent, languages and swap ID. Trocador and its exchanges require
  it, to answer requests from authorities.
  - Each line is **encrypted with a public key** ([age](https://age-encryption.org)).
    The server cannot read it back: only the private key, kept offline, can.
  - One file per day (`YYYY-MM-DD.log`), deleted automatically after **12 months**
    (checked every hour).
  - If the line can't be written, the app doesn't receive the deposit address: no swap
    without its record.
- **No swaps through Tor**: creating a swap is refused (403, `tor_exit`) from Tor exits,
  according to two lists from the Tor Project (the bulk exit list and Onionoo, IPv6
  included), reloaded every 30 minutes. While either list is missing or more than a
  day old, swap creation is refused (503). Rates and swap status still work.
- **VPNs are fine**: only Tor exits are refused. Many people can share one VPN server's
  address, hence generous limits: 120 requests a minute and 20 swap creations per
  10 minutes per address (the app itself stays under 6 requests a minute), and a daily
  cap of swap creations per address (`-trades-per-day`, 100 by default). IPv6 addresses
  are counted per /64. Counters live in memory only and are forgotten at the end of
  each window.
- **No other log**: Go's connection errors (which contain the IP address) are dropped;
  the program only writes its own errors, without addresses or requests.

## The website and its data

- **Static site** on the main domain (`-site-domain`, `-site-dir`), `www.` redirected to
  it, GET and HEAD only, no directory listings, no hidden files, **no access log**. The
  relay only answers on `relay.` (`/api/`, `/health`). No reverse proxy: the client's
  address reaches the program directly, and goes nowhere else.
- **Public data under `/data/`**: the server itself fetches prices (CoinGecko), currency
  rates (the European Central Bank, through Frankfurter) and Monero's crowdfunding
  proposals, and serves the same copy to everyone, so those sources never see users.
  A bad answer from a source keeps the previous copy.
- `/data/tor-check` answers `yes`, `no` or `unknown`: whether the request came through
  Tor. Never the address, never logged.
- **Onion service** (`-onion-listen`, `-onion-address`): the website and `/data/` only,
  on a local address that Tor forwards to. The relay is not mounted there, since every
  request through the onion comes from 127.0.0.1. Over HTTPS, the site sends the
  `Onion-Location` header to Tor Browser.
- **HTTPS built in**: Let's Encrypt certificates, obtained and renewed by the program.
- `GET /health` answers `ok`, for an external uptime check.

## Build and test

```sh
go test ./...
go build -o biscuit-relay .
./biscuit-relay keygen -out /tmp/test-identity.txt    # prints the public key age1...
./biscuit-relay serve -dev 127.0.0.1:8080 -key-file <file with a Trocador key> \
    -recipient age1... -log-dir /tmp/relay-trades
curl 'http://127.0.0.1:8080/api/coins'
```

For the server: `GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o biscuit-relay-linux .`

## Running it (Debian 12 or 13)

1. **Keys, on your own computer**: `./biscuit-relay keygen -out trades-identity.txt`.
   This file is the **private key** of the swap log: it never goes on the server. Keep
   it offline. Without it the log can't be read, not even to answer a legal request.
   The `age1...` line it prints is the public key, for the service.
2. **Server**: a small VPS in the EU or EEA (1 vCPU and 1 GB of memory are enough).
   **Turn off the host's backups and snapshots**: they would copy the swap log outside
   its 12-month rotation. Automatic security updates, SSH by key only, and a firewall
   that only lets in SSH, 80 (certificates) and 443.
3. **Program and Trocador key**: copy the binary to `/usr/local/bin/biscuit-relay` and
   [`deploy/biscuit-relay.service`](deploy/biscuit-relay.service) to
   `/etc/systemd/system/`. Type the key without leaving it in the shell history:

   ```sh
   install -d -m 700 /etc/biscuit-relay
   read -rs K && printf '%s\n' "$K" > /etc/biscuit-relay/trocador-key && unset K
   chmod 600 /etc/biscuit-relay/trocador-key
   ```

4. **Service**: set the domains and the `age1...` public key in the service file, then
   `systemctl daemon-reload && systemctl enable --now biscuit-relay`. It runs as a
   dynamic user in a locked-down sandbox, and only the service can read the key.
5. **Onion service** (optional), in `/etc/tor/torrc`:

   ```
   SocksPort 0
   HiddenServiceDir /var/lib/tor/biscuit_site/
   HiddenServicePort 80 127.0.0.1:8081
   ```

If the relay goes down, only exchange swaps stop: the wallet and atomic swaps carry on.

## Answering a legal request

Trocador forwards requests by email from an **@trocador.app** address.

1. **Check where it comes from**: an email can be forged. Write back to the known
   Trocador address yourself (not with Reply), or check the DKIM signature.
2. Copy the days concerned from `/var/lib/private/biscuit-relay/trades/` to the offline
   computer that holds the private key.
3. Decrypt there and find the swap:
   `./biscuit-relay decrypt -identity trades-identity.txt 2026-09-26.log | grep <swap ID>`
4. Send **only** the line of the swap asked about, then delete the local copies.

## The app's side of the contract

- Same paths and parameters as `https://trocador.app/api/`, without the `API-Key` header.
- Relay errors are JSON: `{"error": code, "message": text}`. The code `tor_exit` (403)
  is **not** a refused key: the app shows it without turning swaps off.
- Trocador answers `{"error": "Invalid API key"}` (HTTP 404) when the key is refused;
  that answer is passed on as it is.
- The user agent and languages the app sends are what gets logged.

## Contributing

Biscuit is made by a small team: we don't take pull requests and can't answer
questions on GitHub. To report a vulnerability, see
[SECURITY.md](https://github.com/BiscuitWallet/Biscuit/blob/main/SECURITY.md).

## License

BSD 3-Clause, see [LICENSE](LICENSE).
