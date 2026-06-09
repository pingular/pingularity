# Security policy

## Reporting a vulnerability

Email **github@cansofgrease.com** with "pingularity security" in the subject.
Please do not open a public issue for anything you believe is exploitable.

Include what you need to make the problem reproducible: the version
(`pingularity version`), the platform, how it was installed, and the steps.
A proof of concept helps but is not required.

Expect an acknowledgement within a week. There is no bug bounty and no payment.
If a fix is warranted it ships in the next release, and you get credit in the
release notes unless you would rather not.

## Supported versions

The latest release only. Pingularity is a single binary with no long-term
support branches, so fixes go into the next tag rather than being backported.

## What is in scope

Anything that lets someone who should not have access read your history, change
your settings, or run code on the host. Reports about the dashboard, the HTTP
API, the update check, the alert webhooks, or how data and secrets are stored
on disk are all wanted.

## What is not

Pingularity is a self-hosted monitor with **no multi-tenant boundary**. It has
one operator, and that operator is trusted. The trust boundary is the host it
runs on and the network you expose it to, so the following are design decisions
rather than vulnerabilities:

- The service runs with the privileges you install it with, and by default that
  means root or Administrator.
- The default listen address is reachable from the network. A default-on access
  filter and an optional password gate it. See the design doc below.
- The binary never terminates TLS. Put a reverse proxy in front of it if it
  needs to be reachable beyond the machine it runs on.
- An authenticated operator can import arbitrary data, change any setting, and
  point alerts at nearly any URL. That is the product working as intended.
  Link-local and cloud-metadata destinations are refused, and the webhook
  client follows no redirects, so a **bypass** of those guards is very much in
  scope even though the general capability is not.

All of these are explained, with the reasoning and the mitigations, in
[docs/security-model.md](docs/security-model.md). Please read it before
reporting one of them. If you think the reasoning there is wrong, that is a
conversation worth having, and the email address above is the right place.
