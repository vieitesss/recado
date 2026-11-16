# Only the side that knows the machine runs commands

Sending to `recado://rpi/sub-1` runs `recado deliver` on `rpi` through the
machine's configured command prefix (`ssh`, `docker exec`, `podman exec`). The
remote side never connects back: a reply addressed upstream is appended to its
outbox, and a relay on the sending machine pulls it over the same transport and
delivers it locally. Letting each side push to the other would be simpler, but
containers and hosts behind NAT usually cannot reach the machine that started
them, and they would need their own registry of how to. The relay therefore
exists only on the side that configured the machine, one per remote machine,
and agents never choose a route: they reply by message id and the envelope
carries the address.
