# cloudflare/awsapigw

The AWS API Gateway front door behind Cloudflare. An HTTP API is reachable
only through Cloudflare: its custom domain demands a client certificate, and
the only certificate it trusts is Cloudflare's origin-pull CA. Everything
else fails the TLS handshake.

```go
d, err := awsapigw.New(ctx, "edge", &awsapigw.Args{
	FrontDoor:     awsapigw.FrontDoor{APIID: apiID, StageName: stage},
	DomainName:    "api.example.com",
	TruststorePEM: originPullCAPEM, // Cloudflare's Authenticated Origin Pulls CA
	Truststore: awsapigw.Truststore{NewBucket: &awsapigw.NewBucketArgs{
		Name: "example-truststore", ApplyPrincipalArns: applyRoleARNs,
	}},
	Certificate: &awsapigw.CertificateArgs{CreateValidationRecord: createRecord},
	DNS: &awsapigw.DNSArgs{Provider: cf, ZoneID: zoneID},
}, pulumi.Provider(awsProvider))
```

## What it creates

| Resource (logical name) | Notes |
|---|---|
| `<name>-certificate`, `<name>-certificate-validation` | Only with `Certificate`: an ACM certificate with DNS validation. The record is made by your callback, so the package holds no DNS credential. With `CertificateArn` you bring the certificate. |
| `<name>-truststore`, `-truststore-encryption`, `-truststore-versioning`, `-truststore-public-access`, `-truststore-policy` | Only with `Truststore.NewBucket`: a protected, encrypted, versioned, private bucket whose policy denies plain HTTP and denies writes under `truststore/` to everyone but `ApplyPrincipalArns`. |
| `<name>-truststore-pem` | The one object, `truststore/client-ca.pem`. The domain pins its version. |
| `<name>-domain` | Regional, TLS 1.2, mutual TLS on the truststore. |
| `<name>-domain-mapping` | The domain to `APIID` / `StageName`. |
| `Args.DNS.ResourceName` (default `<name>-dns`) | Only with `DNS`: the CNAME of the domain to the API Gateway target, made with your Cloudflare provider. |

The logical names are a contract: a stack that has them keeps its resources.
A test pins the set.

## Adopting an existing certificate

A certificate that another stack built (the ACM certificate, its
`CertificateValidation`, the Cloudflare validation record) and the access
CNAME can be moved into the stack that runs `New` with `pulumi state move`
and adopted without a create, delete or replace. Declare each under the name
and parent it had, or alias it from them; the next preview must show no
replace.

```go
cert := &awsapigw.CertificateArgs{
	KeyAlgorithm: "EC_secp384r1",
	// the certificate, top-level in the old stack as "api-cert"
	Name: "api-cert", Aliases: awsapigw.NoParentAliases("api-cert"),
	// its validation, named like the certificate there
	ValidationName: "api-cert", ValidationAliases: awsapigw.NoParentAliases("api-cert"),
	// the DNS-only validation record, top-level as "api-cert-validation"
	ValidationRecord: &awsapigw.ValidationRecordArgs{
		Provider: cf, ZoneID: zoneID,
		ResourceName: "api-cert-validation",
		Aliases:      awsapigw.NoParentAliases("api-cert-validation"),
		Comment:      "ACM DNS validation of access.example.com",
	},
}
dns := &awsapigw.DNSArgs{
	Provider: cf, ZoneID: zoneID,
	ResourceName: "cname-edge-access-example-com",
	// it was under a tunnel component: that component's URN, which need not exist here
	Aliases: []pulumi.Alias{{
		Name:      pulumi.String("cname-edge-access-example-com"),
		ParentURN: pulumi.URN("urn:pulumi:prod::edge::example:cloudflare:Tunnel::tunnel"),
	}},
}
```

| Resource | Arguments |
|---|---|
| Certificate | `Certificate.Name` = old logical name; `Certificate.Aliases` = `NoParentAliases(old)` for a top-level one. `KeyAlgorithm` and `Args.Tags` must equal the old inputs. |
| Validation | `Certificate.ValidationName` = old name; `Certificate.ValidationAliases`. |
| Validation record | `Certificate.ValidationRecord.ResourceName` and `.Aliases`; `.Provider` is the Cloudflare provider of the new stack (a provider's own name, such as an `acm-<name>` one, is not part of a resource's URN, so it needs no alias). The record is DNS-only, automatic TTL, with the name, type and value ACM asks for, trailing dots removed. Set `.Comment` to the old comment, or the preview shows an in-place update. |
| Access CNAME | `DNS.ResourceName` = old name; `DNS.Aliases` with `ParentURN` of the old parent (or `NoParentAliases` if it had none). Proxied, automatic TTL. |

The AWS provider is not part of any of these URNs: pass the provider that
owns the certificate's account as an option to `New`, as for a new one.
Moved resources keep their provider reference only if the new stack's
provider has the same URN; otherwise the first apply re-points them, an update
in place. The access CNAME's content becomes the API Gateway target, which is
an in-place update if the old record pointed elsewhere.

## Authenticated Origin Pulls

The package does not turn on the zone setting. Set `tls_client_auth` on the
zone (`cloudflare/zone`: `AuthenticatedOriginPulls`), and the record must be
proxied, or Cloudflare presents no client certificate and the domain refuses
every request. The `DNS` record is therefore always proxied; `DNS.Unproxied`
is refused by `Validate`. Keep the API's default `execute-api` endpoint
disabled: it does not ask for a client certificate.

## The truststore and its guard

API Gateway reads the truststore from S3 and pins an object version. Whoever
can write that object chooses who may call the API, so the prefix is guarded:
a bucket policy statement (`ProtectedPrefix.Statement`) denies writes under
`truststore/` to every principal whose ARN is not in the apply list (the CD
role, an admin role, a break-glass role; roles, never sessions).

- `Truststore.NewBucket` builds a bucket with that guard.
- `Truststore.Existing{BucketName, Versioned, GuardedPrefix}` uses a bucket you
  own. The package cannot see its configuration, so you attest that it is
  versioned and carries the guard (put `ProtectedPrefix{...}.Statement(bucketARN)`
  in its policy), and it refuses an attestation that is not given.

## Using the certificate and the truststore without a front door

`New` needs the HTTP API. A stack whose API is made by another tool still wants
the certificate and the guarded truststore, and gets them from the two builders
`New` itself uses:

- `NewCertificate(ctx, name, &CertificateArgs{DomainName, ValidationRecord: ...}, opts...)`
  returns the certificate, its validation and `CertificateArn` (the validation's,
  so it resolves only once ACM has issued). `Name`, `ValidationName`,
  `ValidationRecord.ResourceName` and the aliases set the logical names, to adopt
  resources a stack already has.
- `NewTruststore(ctx, name, &TruststoreArgs{PEM, Bucket}, opts...)` returns the
  bucket (when made), the object, `BucketName` and `URI`; the object's `VersionId`
  is what a domain pins. With an empty `name` the resources are named
  `truststore`, `truststore-encryption`, `truststore-versioning`,
  `truststore-public-access`, `truststore-policy` and `truststore-pem`; with a
  name they are `<name>-truststore...`, as inside `New`.

Pass the AWS provider and any parent as `opts`; the validation record takes its
Cloudflare provider from `ValidationRecordArgs.Provider`.

## DNS ownership

DNS is yours unless you pass `DNS`. Without it, point `DomainName` at
`Domain.DomainTarget` (a CNAME) yourself. With it, the record is created with
`DNS.Provider` in `DNS.ZoneID`. To take over a record the stack already has, set
`ResourceName` to its logical name and, if its URN differs, `Aliases`.

## Migrating from sluis

`truvity/sluis` `deploy/pulumi/edge/cloudflare` (`edgecloudflare.NewEdge`)
registered the same resources under the same names. To move without
replacing anything:

1. Build `FrontDoor` from sluis's `Lambda.FrontDoor()` by copying its fields
   (`Name`, `Component`, `APIID`, `StageName`); there is no sluis import. The
   domain and mapping are aliased to `<FrontDoor.Name>-domain` and
   `<FrontDoor.Name>-domain-mapping` under `FrontDoor.Component`, as sluis did.
2. Keep the component's type: pass `ComponentType: awsapigw.ComponentTypeSluis`
   (`sluis:aws:CloudflareEdge`) so the URN is unchanged, with no reliance on
   alias inheritance. Or leave the default
   (`truvity:aws:ApiGatewayDomain`) and pass
   `pulumi.Aliases([]pulumi.Alias{{Type: pulumi.String("sluis:aws:CloudflareEdge")}})`
   as an option to `New`; Pulumi then renames the component and its children.
3. A sluis `Storage` becomes `Truststore.Existing{BucketName, Versioned: true,
   GuardedPrefix: true}` (sluis checked these on the object; now you attest
   them). A sluis `TruststoreBucket` becomes `Truststore.NewBucket`.
4. `PublicCache` (the Cloudflare cache rule) is not part of this package; the
   zone's `public_cache` contribution does it.
5. Run the preview: it must show no replace.
