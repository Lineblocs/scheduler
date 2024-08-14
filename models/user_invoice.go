package models

type UserInvoice struct {
	InvoiceDesc        string `json:"invoice_desc"`
	Id                 int    `json:"id"`
	Cents              int    `json:"cents"`
	ConfirmationNumber int    `json:"confirmation_number"`
}
