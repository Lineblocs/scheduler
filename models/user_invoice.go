package models

type UserInvoice struct {
	InvoiceDesc        string `json:"invoice_desc"`
	Id                 int    `json:"id"`
	Cents              int    `json:"cents"`
	ConfirmationNumber int    `json:"confirmation_number"`
	PaymentMethodId    string `json:"payment_method_id"`
	CardLast4          string  `json:"card_last_4"`
	CardBrand          string  `json:"card_brand"`
}
