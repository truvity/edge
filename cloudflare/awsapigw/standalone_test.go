package awsapigw_test

import (
	"strings"
	"testing"

	"github.com/pulumi/pulumi-cloudflare/sdk/v6/go/cloudflare"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"

	"github.com/truvity/edge/v4/cloudflare/awsapigw"
)

// NewTruststore on its own, with an empty name, makes a stack's resources at
// the top level under the names a stack with only a truststore uses, with the
// same inputs the front door gives them.
func TestNewTruststoreStandalone(t *testing.T) {
	rec, out, err := run(t, func(ctx *pulumi.Context, collect func(string, pulumi.StringInput)) error {
		ts, err := awsapigw.NewTruststore(ctx, "", &awsapigw.TruststoreArgs{
			PEM: truststore, Bucket: newBucket(), Tags: map[string]string{"component": "ci"},
		})
		if err != nil {
			return err
		}
		collect("uri", ts.URI)
		collect("bucket", ts.BucketName)
		collect("version", ts.Object.VersionId)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"truststore", "truststore-encryption", "truststore-pem", "truststore-policy", "truststore-public-access", "truststore-versioning"}
	if got := rec.names(); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("names = %v, want %v", got, want)
	}
	b := rec.one(t, "aws:s3/bucket:Bucket", "truststore")
	if prop(b, "bucket").StringValue() != "acme-truststore" || !prop(b, "tags").ObjectValue()["component"].IsString() {
		t.Errorf("bucket: %v", b.Inputs)
	}
	if !rec.protected["aws:s3/bucket:Bucket/truststore"] {
		t.Error("the bucket is not protected")
	}
	obj := rec.one(t, "aws:s3/bucketObjectv2:BucketObjectv2", "truststore-pem")
	if prop(obj, "key").StringValue() != awsapigw.TruststoreKey || prop(obj, "content").StringValue() != truststore {
		t.Errorf("object: %v", obj.Inputs)
	}
	if out["uri"] != "s3://acme-truststore/"+awsapigw.TruststoreKey || out["bucket"] != "acme-truststore" || out["version"] != "v1" {
		t.Errorf("outputs: %v", out)
	}
}

// With a name the resources carry it, as they do inside New.
func TestNewTruststoreNamed(t *testing.T) {
	rec, _, err := run(t, func(ctx *pulumi.Context, _ func(string, pulumi.StringInput)) error {
		_, err := awsapigw.NewTruststore(ctx, "edge", &awsapigw.TruststoreArgs{PEM: truststore, Bucket: newBucket()})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	rec.one(t, "aws:s3/bucket:Bucket", "edge-truststore")
	rec.one(t, "aws:s3/bucketObjectv2:BucketObjectv2", "edge-truststore-pem")
	rec.one(t, "aws:s3/bucketPolicy:BucketPolicy", "edge-truststore-policy")
}

func TestNewTruststoreExistingBucket(t *testing.T) {
	rec, out, err := run(t, func(ctx *pulumi.Context, collect func(string, pulumi.StringInput)) error {
		ts, err := awsapigw.NewTruststore(ctx, "", &awsapigw.TruststoreArgs{
			PEM:    truststore,
			Bucket: awsapigw.Truststore{Existing: &awsapigw.ExistingBucketArgs{BucketName: "blobs", Versioned: true, GuardedPrefix: true}},
		})
		if err != nil {
			return err
		}
		collect("uri", ts.URI)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if rec.count("aws:s3/bucket:Bucket") != 0 || rec.count("aws:s3/bucketObjectv2:BucketObjectv2") != 1 {
		t.Errorf("resources: %v", rec.names())
	}
	if out["uri"] != "s3://blobs/"+awsapigw.TruststoreKey {
		t.Errorf("uri = %q", out["uri"])
	}
}

func TestNewTruststoreRefusals(t *testing.T) {
	for name, a := range map[string]*awsapigw.TruststoreArgs{
		"nil":          nil,
		"no PEM":       {Bucket: newBucket()},
		"no bucket":    {PEM: truststore},
		"no principal": {PEM: truststore, Bucket: awsapigw.Truststore{NewBucket: &awsapigw.NewBucketArgs{Name: "b"}}},
		"unattested":   {PEM: truststore, Bucket: awsapigw.Truststore{Existing: &awsapigw.ExistingBucketArgs{BucketName: "b"}}},
	} {
		_, _, err := run(t, func(ctx *pulumi.Context, _ func(string, pulumi.StringInput)) error {
			_, err := awsapigw.NewTruststore(ctx, "", a)
			return err
		})
		if err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// NewCertificate on its own: top-level resources under the names the caller
// gives, the validation record DNS-only through the caller's Cloudflare
// provider, the ARN the validation's.
func TestNewCertificateStandalone(t *testing.T) {
	rec, out, err := run(t, func(ctx *pulumi.Context, collect func(string, pulumi.StringInput)) error {
		cf, err := cloudflare.NewProvider(ctx, "cf", &cloudflare.ProviderArgs{ApiToken: pulumi.String("t")})
		if err != nil {
			return err
		}
		c, err := awsapigw.NewCertificate(ctx, "ignored", &awsapigw.CertificateArgs{
			DomainName:   domainName,
			KeyAlgorithm: "EC_secp384r1",
			Tags:         map[string]string{"component": "ci"},
			Name:         "acm-acme", ValidationName: "acm-acme-validation",
			ValidationRecord: &awsapigw.ValidationRecordArgs{
				Provider: cf, ZoneID: pulumi.String("zone1"), ResourceName: "acm-acme-validation-record", Comment: "why",
			},
		})
		if err != nil {
			return err
		}
		collect("arn", c.CertificateArn)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	cert := rec.one(t, "aws:acm/certificate:Certificate", "acm-acme")
	if prop(cert, "domainName").StringValue() != domainName || prop(cert, "keyAlgorithm").StringValue() != "EC_secp384r1" ||
		!prop(cert, "tags").ObjectValue()["component"].IsString() {
		t.Errorf("certificate: %v", cert.Inputs)
	}
	r := rec.one(t, "cloudflare:index/dnsRecord:DnsRecord", "acm-acme-validation-record")
	if prop(r, "name").StringValue() != "_x."+domainName || prop(r, "content").StringValue() != "_y.acm-validations.aws" ||
		prop(r, "type").StringValue() != "CNAME" || prop(r, "proxied").BoolValue() || prop(r, "comment").StringValue() != "why" {
		t.Errorf("record: %v", r.Inputs)
	}
	rec.one(t, "aws:acm/certificateValidation:CertificateValidation", "acm-acme-validation")
	if !strings.Contains(out["arn"], "certificate/requested") {
		t.Errorf("arn = %q", out["arn"])
	}
}

func TestNewCertificateRefusals(t *testing.T) {
	for name, a := range map[string]*awsapigw.CertificateArgs{
		"nil":       nil,
		"no domain": {CreateValidationRecord: func(_ *pulumi.Context, r awsapigw.ValidationRecord) (pulumi.StringInput, error) { return r.Name, nil }},
		"no record": {DomainName: domainName},
	} {
		_, _, err := run(t, func(ctx *pulumi.Context, _ func(string, pulumi.StringInput)) error {
			_, err := awsapigw.NewCertificate(ctx, "x", a)
			return err
		})
		if err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
