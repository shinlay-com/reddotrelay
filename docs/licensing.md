# Licensing and commercial use

RedDotRelay Engine source is licensed under AGPL-3.0-only. The license permits
commercial use, modification, redistribution, self-hosting, and paid services.
Its conditions include providing corresponding source when a modified version
is conveyed or made available for users to interact with over a network.

The `RedDotRelay` name and visual identity are not granted by the software
license. See `TRADEMARKS.md` for permitted descriptive use and the requirements
for modified distributions.

## RedDotRelay Cloud boundary

The commercial RedDotRelay Cloud control plane is a separate work and is not
included in this repository or Engine distribution. It may provision and
operate independently deployed Engine instances through documented Engine
APIs. Cloud-only organization management, hosted identity, billing, quotas,
fleet operations, and proprietary dashboard code are not part of the Engine.

An operator that modifies the AGPL Engine and exposes that modified version to
network users must satisfy the AGPL source-availability requirements for the
modified Engine. Merely charging for an unmodified Engine deployment is not
prohibited by the AGPL.

## Third-party components

Dependencies remain under their respective licenses. `THIRD_PARTY_NOTICES.md`
describes the distribution, and `LICENSES/` contains the version-pinned
inventory and corresponding texts. In particular, imported go-ethereum library
packages are LGPL-3.0-or-later.

This documentation summarizes the intended licensing model; the license texts
govern when there is a conflict.
