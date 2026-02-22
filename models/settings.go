package models

// Settings represents the global configuration fetched from the Lineblocs API
type Settings struct {
	Credentials map[string]string `json:"credentials"`
}

// GetAWSRegion is a helper to safely retrieve the region
func (s *Settings) GetAWSRegion() string {
	return s.Credentials["aws_region"]
}

// GetS3Bucket is a helper to safely retrieve the bucket name
func (s *Settings) GetS3Bucket() string {
	return s.Credentials["s3_bucket"]
}