# Security Policy

## Scope

budgetclaw is a local CLI tool. It reads files under `$HOME/.claude/projects/`, writes to its XDG directories, and sends SIGTERM to processes named `claude`. It does not touch API traffic, does not read prompts or responses, does not open network sockets except for outbound ntfy alerts to the endpoint you configure, and does not require elevated privileges.

A security issue is anything that violates the above pledge: accessing files outside `$HOME/.claude/projects/`, SIGTERMing non-matching processes, leaking data over the network, or enabling privilege escalation.

## Reporting a vulnerability

**Do not file a public issue.**

Send the details to **security@roninforge.org** with:

- A description of the issue and its impact
- Steps to reproduce
- Affected versions
- Your name and whether you want credit in the advisory

You will get an acknowledgement within 72 hours. We aim to have a patched release available within 14 days for high-severity issues and 30 days for lower-severity ones. The embargo window is 90 days.

## Supported versions

Only the latest minor release on the `main` branch receives security fixes. There is no LTS track yet.

## No bug bounty

budgetclaw is a small OSS project. We cannot pay bounties. We will credit responsible disclosures in the release notes and the advisory.
