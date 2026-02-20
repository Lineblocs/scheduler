package models

// Workspace represents a workspace entity
type Workspace struct {
	Id        int
	CreatorId int
	Plan      string
}

// User represents a user entity
type User struct {
	Id int
}

// ServicePlan represents a service plan with pricing and limits
type ServicePlan struct {
	BaseCosts       int64
	MinutesPerMonth int64
	RecordingSpace  int64
	Fax             int
	PayAsYouGo      bool
}

// BillingInfo contains billing information for a workspace
type BillingInfo struct {
	InvoiceDue            string
	RemainingBalanceCents int64
}

// BaseCosts contains base cost rates for various services
type BaseCosts struct {
	RecordingsPerByte int64
	FaxPerUsed        float64
}

// Call represents a call record
type Call struct {
	Id             int
	DurationNumber int
}

// DID represents a direct inward dial number
type DID struct {
	Id          int
	MonthlyCost int
}