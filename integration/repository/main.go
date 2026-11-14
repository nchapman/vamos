// Command repository administers only the disposable guest's loopback S3 fixture.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/nchapman/soar/internal/backupbackend"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
func run() error {
	if len(os.Args) != 2 && len(os.Args) != 3 {
		return fmt.Errorf("usage: repository create|list|probe PREFIX (credentials on stdin)")
	}
	var secret backupbackend.Credentials
	if err := json.NewDecoder(os.Stdin).Decode(&secret); err != nil {
		return err
	}
	if os.Args[1] == "probe" && len(os.Args) == 3 {
		dir, err := os.MkdirTemp("/run", "repository-probe-")
		if err != nil {
			return err
		}
		defer os.RemoveAll(dir)
		id, err := backupbackend.New("/", dir).Probe(context.Background(), backupbackend.Config{Endpoint: "https://127.0.0.1:19001", Bucket: "soar-acceptance", Prefix: os.Args[2], Region: "us-east-1", Lookup: "path"}, secret)
		if err != nil {
			return fmt.Errorf("read-only probe failed")
		}
		return json.NewEncoder(os.Stdout).Encode(id)
	}
	client := s3.New(s3.Options{Region: "us-east-1", BaseEndpoint: aws.String("http://127.0.0.1:19000"), UsePathStyle: true, Credentials: credentials.NewStaticCredentialsProvider(secret.AccessKeyID, secret.SecretAccessKey, "")})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	bucket := aws.String("soar-acceptance")
	switch os.Args[1] {
	case "create":
		_, err := client.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: bucket})
		return err
	case "list":
		// No prefix filter: includes locks and writes outside the repository.
		pages := s3.NewListObjectsV2Paginator(client, &s3.ListObjectsV2Input{Bucket: bucket})
		objects := map[string]string{}
		for pages.HasMorePages() {
			page, e := pages.NextPage(ctx)
			if e != nil {
				return e
			}
			for _, o := range page.Contents {
				objects[aws.ToString(o.Key)] = fmt.Sprintf("%s/%d/%s", aws.ToString(o.ETag), aws.ToInt64(o.Size), o.LastModified.UTC().Format(time.RFC3339Nano))
			}
		}
		return json.NewEncoder(os.Stdout).Encode(objects)
	}
	return fmt.Errorf("unknown fixture action")
}
