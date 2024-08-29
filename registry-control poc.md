# registry-control poc

This is a proof-of-concept that we can sign & acknowledge registry-control
assertions as I learn the _snapd_ codebase.\
Just in case you check the `diff` and wince (`git diff master..registry-control-assertion`),
this is throwaway & hacked-together code. We'll do everything correctly soonest :).

## Build & install the snap

Pull the `registry-control-assertion` branch of this fork and then build the snap:

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

First, we have to enable the `registry` & `registry-control` feature flags. \
If we try signing right now, we'll get an error:

```console
$ sudo snap get system experimental -d
{
        "experimental": {
                "registries": true,
                "registry-control": false
        }
}

$ sudo curl -s --unix-socket /run/snapd.socket http://localhost/v2/sign \
    -X POST --data "$(cat registry-control.json)" | jq .result
{
  "message": "\"registry-control\" feature flag is disabled: set 'experimental.registry-control' to true (api)"
}

$ sudo snap set system experimental.registry-control=true
```

Next, sign the assertion using the API:

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
sign-key-sha3-384: t9yuKGLyiezBq_PXMJZsGdkTukmL7MgrgqXAlxxiZF4TYryOjZcy48nnjDmEHQDp

AcLBUgQAAQoABgUCZtF51gAAbo0QAEsurlwSNIsSW3MMd+GI79fqXvGbhnBE3bt/EJeuJeXmyk8l
lKOQZ3ZQygz5w3ZDeLVlEamjSxmvoJzbUtpLGtCwDGrro2vT4EAICvxVg21+ynsf1ulHO1GoS6Ge
98qqOVtsrFc/ncDiNJuYr4BqCzlFxeLHJnchVcVHBHb07ND9jwzZJExxMIvUW+EJsxpL2ni8N6nD
3ZKIcSQvFCaZTePmalY+AFZ0hTY8SvB0LE9DGhVbg4GtgBDZSXNO3u9clgGA3w5mTJm0spo/+9BU
GgWAz2BvA5L/RP9vxbG2Y8q44yj7fJ2tzF4g2Dj5IOkPR5b2rQHY/lWKpai3mEjJ+6ZyMEVdDMIa
O34fBB+subgADX2JdXot1vvC0U4GCbg5ShoZ2P+ynzde2PKAyxk94+457Jnw5A5BxTyH5wGlCqks
D5jzHRrypR4zaVXQhEcf6g3DsCSgLhAyqO+e51Nz/2SElyRiKGjVlGKNZKBMbGrC6vqAJxiX1vCl
/dcWAlmplJGIZWLrgzM0MZObnr7vReYUc/1Xb+ksXllVWcKwEfWIDhi5LDp2Ez2QquTy0yhozRkm
84AuLYG4nlt/EMkxOHbGHFSCZxKD39Ws31UkBTlxmruCkdI4wna+d+z0J5esGmXuhrG4OQlS/Vlt
xPhuNdKk1wnNwl5THvD1fFwCI7/R
```

We can double check that the assertion's signing key (`sign-key-sha3-384`) is
the same as the device-key (`device-key-sha3-384`):

```console
$ snap known serial
[...]
device-key-sha3-384: t9yuKGLyiezBq_PXMJZsGdkTukmL7MgrgqXAlxxiZF4TYryOjZcy48nnjDmEHQDp
timestamp: 2024-08-29T13:42:13.874988Z
sign-key-sha3-384: wrfougkz3Huq2T_KklfnufCC0HzG7bJ9wP99GV0FF-D3QH3eJtuSRlQc2JhrAoh1
[...]
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
$ curl -s --unix-socket /run/snapd.socket http://localhost/v2/assertions/registry-control -X GET
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
sign-key-sha3-384: t9yuKGLyiezBq_PXMJZsGdkTukmL7MgrgqXAlxxiZF4TYryOjZcy48nnjDmEHQDp

AcLBUgQAAQoABgUCZtF51gAAbo0QAEsurlwSNIsSW3MMd+GI79fqXvGbhnBE3bt/EJeuJeXmyk8l
lKOQZ3ZQygz5w3ZDeLVlEamjSxmvoJzbUtpLGtCwDGrro2vT4EAICvxVg21+ynsf1ulHO1GoS6Ge
98qqOVtsrFc/ncDiNJuYr4BqCzlFxeLHJnchVcVHBHb07ND9jwzZJExxMIvUW+EJsxpL2ni8N6nD
3ZKIcSQvFCaZTePmalY+AFZ0hTY8SvB0LE9DGhVbg4GtgBDZSXNO3u9clgGA3w5mTJm0spo/+9BU
GgWAz2BvA5L/RP9vxbG2Y8q44yj7fJ2tzF4g2Dj5IOkPR5b2rQHY/lWKpai3mEjJ+6ZyMEVdDMIa
O34fBB+subgADX2JdXot1vvC0U4GCbg5ShoZ2P+ynzde2PKAyxk94+457Jnw5A5BxTyH5wGlCqks
D5jzHRrypR4zaVXQhEcf6g3DsCSgLhAyqO+e51Nz/2SElyRiKGjVlGKNZKBMbGrC6vqAJxiX1vCl
/dcWAlmplJGIZWLrgzM0MZObnr7vReYUc/1Xb+ksXllVWcKwEfWIDhi5LDp2Ez2QquTy0yhozRkm
84AuLYG4nlt/EMkxOHbGHFSCZxKD39Ws31UkBTlxmruCkdI4wna+d+z0J5esGmXuhrG4OQlS/Vlt
xPhuNdKk1wnNwl5THvD1fFwCI7/R
```

### GET with CLI

```console
$ snap known registry-control
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
sign-key-sha3-384: t9yuKGLyiezBq_PXMJZsGdkTukmL7MgrgqXAlxxiZF4TYryOjZcy48nnjDmEHQDp

AcLBUgQAAQoABgUCZtF51gAAbo0QAEsurlwSNIsSW3MMd+GI79fqXvGbhnBE3bt/EJeuJeXmyk8l
lKOQZ3ZQygz5w3ZDeLVlEamjSxmvoJzbUtpLGtCwDGrro2vT4EAICvxVg21+ynsf1ulHO1GoS6Ge
98qqOVtsrFc/ncDiNJuYr4BqCzlFxeLHJnchVcVHBHb07ND9jwzZJExxMIvUW+EJsxpL2ni8N6nD
3ZKIcSQvFCaZTePmalY+AFZ0hTY8SvB0LE9DGhVbg4GtgBDZSXNO3u9clgGA3w5mTJm0spo/+9BU
GgWAz2BvA5L/RP9vxbG2Y8q44yj7fJ2tzF4g2Dj5IOkPR5b2rQHY/lWKpai3mEjJ+6ZyMEVdDMIa
O34fBB+subgADX2JdXot1vvC0U4GCbg5ShoZ2P+ynzde2PKAyxk94+457Jnw5A5BxTyH5wGlCqks
D5jzHRrypR4zaVXQhEcf6g3DsCSgLhAyqO+e51Nz/2SElyRiKGjVlGKNZKBMbGrC6vqAJxiX1vCl
/dcWAlmplJGIZWLrgzM0MZObnr7vReYUc/1Xb+ksXllVWcKwEfWIDhi5LDp2Ez2QquTy0yhozRkm
84AuLYG4nlt/EMkxOHbGHFSCZxKD39Ws31UkBTlxmruCkdI4wna+d+z0J5esGmXuhrG4OQlS/Vlt
xPhuNdKk1wnNwl5THvD1fFwCI7/R
```

## Clean-up

Roll back to the correct `snapd` version:

```console
$ snap refresh snapd --stable --amend
```
