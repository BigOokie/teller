# teller

## Setup project

### Prerequest

* Have go1.8+ installed
* Have `GOPATH` env set
* Have btcd started
* Have skycoin node started

### Start teller-proxy

```bash
cd cmd/proxy/
go run proxy.go
```

once the proxy start, will show a `pubkey` in the log.

```bash
18:28:49 proxy.go:33: Pubkey: 03583bf0a6cbe7048023be5475aca693e192b4b5570bcc883c50f7d96f0a996eda
```

### Start teller

install the `skycoin-cli`

```bash
cd cmd/teller
./install-skycoin-cli.sh
```

add pregenearted bitcoin deposit address list in `btc_addresses.json`.

```bash
{
    "btc_addresses": [
        "1PZ63K3G4gZP6A6E2TTbBwxT5bFQGL2TLB",
        "14FG8vQnmK6B7YbLSr6uC5wfGY78JFNCYg",
        "17mMWfVWq3pSwz7BixNmfce5nxaD73gRjh",
        "1Bmp9Kv9vcbjNKfdxCrmL1Ve5n7gvkDoNp"
    ]
}
```

use `tool` to pregenerate bitcoin address list:

```bash
cd cmd/tool
go run tool.go newbtcaddress $seed $num
```

example:

```bash
go run tool.go newbtcaddress 12323 3

2Q5sR1EgTWesxX9853AnNpbBS1grEY1JXn3
2Q8y3vVAqY8Q3paxS7Fz4biy1RUTY5XQuzb
216WfF5EcvpVk6ypSRP3Lg9BxqpUrgBJBco
```


teller's config is managed in `config.json`, need to set the `wallet_path`
in `skynode` field to an absolute path of skycoin wallet file, and set up the `btcd`
config in `btc_rpc` field, including server address, username, password and
absolute path to the cert file.

config.json:

```json
{
    "proxy_address": "127.0.0.1:7070",
    "reconnect_time": 5,
    "dial_timeout": 5,
    "ping_timeout": 5,
    "pong_timeout": 10,
    "exchange_rate": 500,
    "skynode": {
        "rpc_address": "127.0.0.1:6430",
        "wallet_path": "absolute path to the wallet file"
    },
    "btc_scan": {
        "check_period": 20,
        "deposit_buffer_size": 1024
    },
    "btc_rpc": {
        "server": "127.0.0.1:8334",
        "user": "",
        "pass": "",
        "cert": "absolute path to rpc cert file"
    },
    "sky_sender": {
        "request_buffer_size": 1024 
    }
}
```

run teller service

```bash
go run teller.go -proxy-pubkey=$the_pubkey_of_proxy
```

## Service apis

The http apis service is provided by the proxy and serve on port 7071.

### Bind

```bash
Method: GET
URI: /bind
Args: skyaddr
```

example:

```bash
curl http://localhost:7071/bind?skyaddr=t5apgjk4LvV9PQareTPzWkE88o1G5A55FW
```

response:

```bash
{
    "btc_address": "1Bmp9Kv9vcbjNKfdxCrmL1Ve5n7gvkDoNp"
}
```

### Status

```bash
Method: GET
URI: /status
Args: skyaddr
```

example:

```bash
curl http://localhost:7071/status?skyaddr=t5apgjk4LvV9PQareTPzWkE88o1G5A55FW
```

response:

```bash
{
    "statuses": [
        {
            "seq": 1,
            "update_at": 1501137828,
            "status": "done"
        },
        {
            "seq": 2,
            "update_at": 1501128062,
            "status": "waiting_deposit"
        },
        {
            "seq": 3,
            "update_at": 1501128063,
            "status": "waiting_deposit"
        },
    ]
}
```