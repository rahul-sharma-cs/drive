// Package blob owns Drive's S3 wiring. Every process that talks to the object
// store — the server, infra-init and the day-0 spike — builds its client here,
// so there is exactly one place where the Garage-specific settings live.
package blob

import (
	"context"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/rahul-sharma-cs/drive/server/internal/config"
)

// Region must equal garage.toml's s3_api.s3_region; a mismatch signs requests
// that Garage then rejects with SignatureDoesNotMatch.
const Region = "garage"

// New returns the shared S3 client and its presigner.
//
// Never set ChecksumAlgorithm on any request built from this client: the
// checksum settings below keep SDK checksum parameters out of presigned
// queries, which is what makes presigned PUTs work against Garage.
func New(ctx context.Context, cfg *config.Config) (*s3.Client, *s3.PresignClient, error) {
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx,
		awsconfig.WithRegion(Region),
		awsconfig.WithCredentialsProvider(
			credentials.NewStaticCredentialsProvider(cfg.S3AccessKey, cfg.S3SecretKey, ""),
		),
		awsconfig.WithRequestChecksumCalculation(aws.RequestChecksumCalculationWhenRequired),
		awsconfig.WithResponseChecksumValidation(aws.ResponseChecksumValidationWhenRequired),
	)
	if err != nil {
		return nil, nil, fmt.Errorf("aws config: %w", err)
	}

	client := s3.NewFromConfig(awsCfg, func(o *s3.Options) {
		o.BaseEndpoint = aws.String(cfg.S3Endpoint)
		// Virtual-hosted addressing (bucket.localhost) does not resolve.
		o.UsePathStyle = true
	})

	return client, s3.NewPresignClient(client), nil
}
