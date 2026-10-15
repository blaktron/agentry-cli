# Windows code signing: what is pinned here

`microsoft-identity-verification-root-2020.pem` is the root of the chain
every Azure Artifact Signing certificate, and its timestamp, chains to:
`CN=Microsoft Identity Verification Root Certificate Authority 2020`, SHA-256
fingerprint `53:67:F2:0C:7A:DE:0E:2B:CA:79:09:15:05:6D:08:6B:72:0C:33:C1:FA:2A:26:61:AC:F7:87:E3:29:2E:12:70`,
valid to 2045-04-16, published at
<https://www.microsoft.com/pkiops/docs/repository.htm>. The desktop
repository commits the same file. It is committed because it is not in the
Mozilla bundle that Ubuntu's `ca-certificates` ships, so `osslsigncode verify`
has to be handed it. The fingerprint is enforced in
`scripts/check-windows-binary.sh` (`pinned_fp`) before the file is trusted
with anything, and `scripts/acceptance/windows-signing.sh` checks that the
file and the pin agree.

`scripts/sign-windows.sh` pins the `jsign` release that produces the signature
(version and SHA-256 of the jar). Changing any pin is a pull request.

The release workflow signs the Windows executable against Azure Artifact
Signing with `jsign`. The job authenticates with the OIDC token GitHub issues
for it in the `release` environment, which `scripts/signing-token.sh`
exchanges for an Azure access token, so no Azure secret is stored. The
signature is then read back with `osslsigncode` against the pinned root and
the signer name in `build/release.env`.

None of this is needed to build or run the CLI. It belongs to the release
workflow and to the signing acceptance script.
