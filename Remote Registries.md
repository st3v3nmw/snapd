# Remote Registries

## Build & install the snap

```console
$ snapcraft
Installed package repositories
Generated snap metadata
Created snap package snapd_2.64+git175.gd32a5ae-dirty_amd64.snap

$ sudo snap install snapd_2.64+git175.gd32a5ae-dirty_amd64.snap --dangerous
2024-08-29T11:05:46+03:00 INFO Waiting for automatic snapd restart...
snapd 2.64+git175.gd32a5ae-dirty installed
```

## Create the assertion

Create a JSON assertion named `registry-control.json` with the following content:

```json
{
    "type": "registry-control",
    "operator-id": "f22PSauKuNkwQTM9Wz67ZCjNACuSjjhN",
    "views": [
        {
            "name": "f22PSauKuNkwQTM9Wz67ZCjNACuSjjhN/network/control-device"
        },
        {
            "name": "f22PSauKuNkwQTM9Wz67ZCjNACuSjjhN/network/observe-device"
        },
        {
            "name": "f22PSauKuNkwQTM9Wz67ZCjNACuSjjhN/network/control-interfaces"
        },
        {
            "name": "f22PSauKuNkwQTM9Wz67ZCjNACuSjjhN/network/observe-interfaces"
        }
    ]
}
```

## Sign the assertion

```console
$ sudo curl -s --unix-socket /run/snapd.socket http://localhost/v2/sign \
    -X POST --data "$(cat registry-control.json)" | jq .result | \
    awk '{gsub(/\\n/,"\n")}1' | awk '{gsub(/"/,"")}1' > registry-control.assert
```

The `registry-control.assert` assertion should look like:

```yaml
type: registry-control
operator-id: f22PSauKuNkwQTM9Wz67ZCjNACuSjjhN
views:
  -
    name: f22PSauKuNkwQTM9Wz67ZCjNACuSjjhN/network/control-device
  -
    name: f22PSauKuNkwQTM9Wz67ZCjNACuSjjhN/network/observe-device
  -
    name: f22PSauKuNkwQTM9Wz67ZCjNACuSjjhN/network/control-interfaces
  -
    name: f22PSauKuNkwQTM9Wz67ZCjNACuSjjhN/network/observe-interfaces
sign-key-sha3-384: zU_ILTqVH-eXp7QNGYfiap3wLn4Z8JCqI6s_fxlEq1clElVrRSyvNo89YTRqMKvU

AcLBUgQAAQoABgUCZtAsXQAA6IUQAEl2eYCCG5q7/WoaMwcOsthoQ4cKhKUqCHbka8ATTAA3/H37
TMxHDEvKFI8b4e5UPBIqC+SZlQkyI2gfrUnueBwyFgxwbt6Hvhjk2cW8i/KYWC5p6gImo2JXgyTU
JeuOqubxptYcVFg8a6HF/q4vXWtcXiz7DsmOqAlb5M86WFBBE4xxe2emanRyxW3sjFI5KXuuHceX
jvjFQzEgZA0BniwrHvuF6EHbN+O3fvb3FdWaUe+frvpcqCTglHv7ka2f4EbkmpovgUf6w7umQBRU
JmLKat5skjCtdF9brxiQM4cCZKdXpgiZFmEPYZtzlhHHxyH7UWN5j5087PB6KUGbJFyKT2xvJNAM
Yl58DTsu0Vmt2kOuFHiL6NtonNO4MOdLdFoH/iyNYP1A/wgTgl7yhr5PmD8JY4YzUfyLpZ1lttSE
SsL7hMsGTptGDHiHejWFs0brwGYoXL87fKt5qd0nugptC6uvsGdVr5GbVUwhIAJaGsiw/Ut6yNta
YWPPw3NyTq6fBwT+F7/9eWpBxcwqlLjMAngF0Dublo4FldhV8QcA4tUGab+z6xwFNFrSf80hj+52
HUhhxct3LGzR7YNpIKl7ZCr9/hDNfYsAQushjhs7FO4i1lMtr5W/oLhVi+VCwMV4lm/kjTfjkdhA
zR/dTqMPfJxGxUzjInOikQEdHVfa
```

// TODO

Alternatively, you can sign the assertion with the CLI:

```console
$ snap sign ...
```

## Acknowledge the assertion

```console
$ snap ack registry-control.assert
```

## Fetch the assertion

### `registry-control` is a known assertion type

```console
$ curl -s --unix-socket /run/snapd.socket http://localhost/v2/assertions -X GET | jq .result
{
  "types": [
    "account",
    "account-key",
    "account-key-request",
    "base-declaration",
    "device-session-request",
    "model",
    "preseed",
    "registry",
    "registry-control",
    "repair",
    "serial",
    "serial-request",
    "snap-build",
    "snap-declaration",
    "snap-developer",
    "snap-resource-pair",
    "snap-resource-revision",
    "snap-revision",
    "store",
    "system-user",
    "validation",
    "validation-set"
  ]
}
```

### GET with API

```console
$ curl ...
```

### GET with CLI

```console
$ snap known registry-control
```

## Clean-up

```console
$ sudo snap ...
```
