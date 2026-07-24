package types

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestBuildContentDisposition_ASCII covers the simple ASCII case where the
// legacy `filename="..."` form is emitted.
func TestBuildContentDisposition_ASCII(t *testing.T) {
	got := BuildContentDisposition("cat.jpg")
	assert.Equal(t, `attachment; filename="cat.jpg"`, got)
}

// TestBuildContentDisposition_Empty returns empty so callers know to skip the
// header entirely. Setting an empty value would override the cloud default,
// which usually means "inline display" — different from "no override".
func TestBuildContentDisposition_Empty(t *testing.T) {
	assert.Equal(t, "", BuildContentDisposition(""))
}

// TestBuildContentDisposition_WithQuote verifies embedded quotes are escaped
// so the filename attribute parses cleanly per RFC 6266.
func TestBuildContentDisposition_WithQuote(t *testing.T) {
	got := BuildContentDisposition(`cat"safe".jpg`)
	assert.Equal(t, `attachment; filename="cat\"safe\".jpg"`, got)
}

// TestBuildContentDisposition_NonASCII uses the RFC 5987 filename* form so
// browsers correctly decode UTF-8 filenames (Safari and old Chrome require
// this; they garble the legacy filename attribute on non-ASCII).
func TestBuildContentDisposition_NonASCII(t *testing.T) {
	got := BuildContentDisposition("猫咪.jpg")
	assert.Equal(t, `attachment; filename*=UTF-8''%E7%8C%AB%E5%92%AA.jpg`, got)
}

// TestBuildContentDisposition_MixedASCIIAndNonASCII verifies any non-ASCII
// code point in the string triggers the filename* form, even if most of the
// string is ASCII.
func TestBuildContentDisposition_MixedASCIIAndNonASCII(t *testing.T) {
	got := BuildContentDisposition("file-猫咪.txt")
	assert.Contains(t, got, "filename*=UTF-8''")
	assert.NotContains(t, got, `filename="`, "must not use legacy form when non-ASCII present")
}

// TestPutObjectActionForVendor verifies the per-vendor action prefix. Cloud
// STS engines reject policies whose action prefix doesn't match the vendor's
// namespace, so a wrong mapping silently breaks uploads at STS mint time.
func TestPutObjectActionForVendor(t *testing.T) {
	cases := []struct {
		name   string
		vendor int32
		want   string
	}{
		{"aliyun_oss", vendorAliyunOSS, "oss:PutObject"},
		{"aws_s3", vendorAWSS3, "s3:PutObject"},
		{"s3_compatible", vendorS3Compatible, "s3:PutObject"},
		{"tencent_cos", vendorTencentCOS, "name/cos:PutObject"},
		{"huawei_obs", vendorHuaweiOBS, "obs:object:PutObject"},
		{"volcengine_tos", vendorVolcengineTOS, "tos:PutObject"},
		{"unknown_defaults_to_s3", 99, "s3:PutObject"},
		{"unspecified_defaults_to_s3", 0, "s3:PutObject"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, PutObjectActionForVendor(tc.vendor))
		})
	}
}
