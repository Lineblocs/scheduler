package utils

import (
    "bytes"
    "crypto/rand"
    "database/sql"
    "encoding/json"
    "fmt"
    "io"
    "net/http"
    "os"
    "strconv"
    "time"
    "math"

    helpers "github.com/Lineblocs/go-helpers"
    _ "github.com/go-sql-driver/mysql"
    "github.com/joho/godotenv"
    _ "github.com/mailgun/mailgun-go/v4"
    "github.com/sirupsen/logrus"
    "github.com/CyCoreSystems/ari/v5"
    "github.com/CyCoreSystems/ari/v5/client/native"
    billing "lineblocs.com/scheduler/handlers/billing"
    models "lineblocs.com/scheduler/models"
)

var db *sql.DB

type DBConn struct {
    Conn *sql.DB
}

type BillingParams struct {
    Data     map[string]string
    Provider string
}

func NewDBConn(db *sql.DB) *DBConn {
    if db == nil {
        db, _ = helpers.CreateDBConn()
    }
    return &DBConn{
        Conn: db,
    }
}

func GetDBConnection() (*sql.DB, error) {
    if db != nil {
        return db, nil
    }
    var err error
    db, err = helpers.CreateDBConn()
    if err != nil {
        return nil, err
    }
    return db, nil
}

// GetSettingsFromAPI fetches global credentials and bucket info from the internal API
func GetSettingsFromAPI() (*models.Settings, error) {
    apiUrl := os.Getenv("API_URL") + "/user/getSettings"
    apiKey := os.Getenv("LINEBLOCS_KEY")

    req, err := http.NewRequest("GET", apiUrl, nil)
    if err != nil {
        return nil, err
    }

    req.Header.Set("X-Lineblocs-Api-Token", apiKey)
    req.Header.Set("Content-Type", "application/json")

    client := &http.Client{}
    resp, err := client.Do(req)
    if err != nil {
        return nil, err
    }
    defer resp.Body.Close()

    if resp.StatusCode != http.StatusOK {
        return nil, fmt.Errorf("API returned status: %d", resp.StatusCode)
    }

    body, err := io.ReadAll(resp.Body)
    if err != nil {
        return nil, err
    }

    var settings models.Settings
    if err := json.Unmarshal(body, &settings); err != nil {
        return nil, err
    }

    return &settings, nil
}

// CreateARIConnection initializes a connection to the Asterisk ARI server
func CreateARIConnection() (*ari.Client, error) {
    fmt.Println("Connecting to ARI: " + os.Getenv("ARI_URL"))
    
    cl, err := native.Connect(&native.Options{
        Application:  os.Getenv("ARI_RECORDING_APP"),
        Username:     os.Getenv("ARI_USERNAME"),
        Password:     os.Getenv("ARI_PASSWORD"),
        URL:          os.Getenv("ARI_URL"),
        WebsocketURL: os.Getenv("ARI_WSURL"),
    })

    if err != nil {
        fmt.Println("Failed to build native ARI client", "error", err)
        return nil, err
    }

    fmt.Println("Connected to ARI server successfully.")
    return &cl, nil
}

func ChargeCustomer(dbConn *sql.DB, billingParams *BillingParams, user *helpers.User, workspace *helpers.Workspace, invoice *models.UserInvoice) error {
    var hndl billing.BillingHandler
    retryAttempts, err := strconv.Atoi(billingParams.Data["retry_attempts"])
    if err != nil {
        helpers.Log(logrus.InfoLevel, fmt.Sprintf("variable retryAttempts is setup incorrectly. retryAttempts=%s setting value to 0", billingParams.Data["retry_attempts"]))
        retryAttempts = 0
    }

    switch billingParams.Provider {
    case "stripe":
        key := billingParams.Data["stripe_key"]
        hndl = billing.NewStripeBillingHandler(dbConn, key, retryAttempts)
        _, err = hndl.ChargeCustomer(user, workspace, invoice)
    case "braintree":
        key := billingParams.Data["braintree_api_key"]
        hndl = billing.NewBraintreeBillingHandler(dbConn, key, retryAttempts)
        _, err = hndl.ChargeCustomer(user, workspace, invoice)
    }

    return err
}

func GetRowCount(rows *sql.Rows) (int, error) {
    var count int
    for rows.Next() {
        err := rows.Scan(&count)
        if err != nil {
            return 0, err
        }
    }
    return count, nil
}

func DispatchEmail(subject string, emailType string, user *helpers.User, workspace *helpers.Workspace, emailArgs map[string]string) error {
    url := "http://com/api/sendEmail"
    to := user.Email
    email := models.Email{User: *user, Workspace: *workspace, Subject: subject, To: to, EmailType: emailType, Args: emailArgs}
    b, err := json.Marshal(email)
    if err != nil {
        return err
    }
    req, _ := http.NewRequest("POST", url, bytes.NewBuffer(b))
    req.Header.Set("X-Lineblocs-Key", "xxx")
    req.Header.Set("Content-Type", "application/json")

    client := &http.Client{}
    resp, err := client.Do(req)
    if err != nil {
        return err
    }
    defer resp.Body.Close()
    return nil
}

func GetPlan(plans []helpers.ServicePlan, workspace *helpers.Workspace) *helpers.ServicePlan {
    for _, target := range plans {
        if target.KeyName == workspace.Plan {
            return &target
        }
    }
    return nil
}

func GetPlanBySubscription(plans []helpers.ServicePlan, subscription *helpers.Subscription) *helpers.ServicePlan {
    for _, target := range plans {
        if target.Id == subscription.CurrentPlanId {
            return &target
        }
    }
    return nil
}

func (c *DBConn) GetBillingParams() (*BillingParams, error) {
    row := c.Conn.QueryRow("SELECT payment_gateway FROM customizations")
    var paymentGateway string
    if err := row.Scan(&paymentGateway); err != nil {
        return nil, err
    }

    row = c.Conn.QueryRow("SELECT stripe_private_key FROM api_credentials")
    var stripePrivateKey string
    if err := row.Scan(&stripePrivateKey); err != nil {
        return nil, err
    }

    data := make(map[string]string)
    data["stripe_key"] = stripePrivateKey
    data["retry_attempts"] = "0"
    return &BillingParams{Provider: "stripe", Data: data}, nil
}

func Config(key string) string {
    if os.Getenv("USE_DOTENV") != "off" {
        _ = godotenv.Load(".env")
    }
    return os.Getenv(key)
}

func ComputeAmountToCharge(fullCentsToCharge float64, availMinutes float64, minutes float64) (float64, error) {
    minAfterDebit := availMinutes - minutes
    if availMinutes > 0 && minAfterDebit < 0 && availMinutes <= minutes {
        percentOfDebit, err := strconv.ParseFloat(fmt.Sprintf(".%s", strconv.FormatFloat((minutes-availMinutes), 'f', -1, 64)), 64)
        if err != nil {
            return 0, err
        }
        centsToCharge := math.Abs(fullCentsToCharge * percentOfDebit)
        return math.Max(1, centsToCharge), nil
    } else if availMinutes >= minutes {
        return 0, nil
    } else if availMinutes <= 0 {
        return fullCentsToCharge, nil
    }
    return 0, fmt.Errorf("billing error: computeAmountToCharge logic failure")
}

func CreateMonthlyNumberRentalDebit(db *sql.DB, workspaceId int, userId int, start time.Time) error {
    var didId int
    var monthlyCosts int
    results1, err := db.Query("SELECT id, monthly_cost FROM did_numbers WHERE workspace_id = ?", workspaceId)
    if err != nil {
        return err
    }
    defer results1.Close()
    for results1.Next() {
        err = results1.Scan(&didId, &monthlyCosts)
        if err != nil {
            return err
        }
        deduplicationKey := helpers.GenerateDeduplicationKey("NUMBER_RENTAL", start.Year(), int(start.Month()), start.Day(), workspaceId, didId)
        var count int
        err = db.QueryRow("SELECT COUNT(*) FROM users_debits WHERE deduplication_key = ?", deduplicationKey).Scan(&count)
        if err != nil {
            return err
        }
        if count > 0 {
            helpers.Log(logrus.InfoLevel, fmt.Sprintf("Deduplication key %s already exists, skipping debit creation.", deduplicationKey))
            continue
        }
        helpers.Log(logrus.InfoLevel, fmt.Sprintf("Creating debit for number rental (DID ID: %d) for workspace %d", didId, workspaceId))
        stmt, err := db.Prepare("INSERT INTO users_debits (`source`, `status`, `cents`, `module_id`, `user_id`, `workspace_id`, `created_at`, `deduplication_key`) VALUES (?, ?, ?, ?, ?, ?, ?, ?)")
        if err != nil {
            return err
        }
        defer stmt.Close()
        _, err = stmt.Exec("NUMBER_RENTAL", "PENDING", monthlyCosts, didId, userId, workspaceId, start, deduplicationKey)
        if err != nil {
            return err
        }
    }

    return nil
}

func GetWorkspaceUserCount(db *sql.DB, workspaceId int) int {
    rows, err := db.Query("SELECT COUNT(*) as count FROM workspaces_users WHERE workspace_id = ?", workspaceId)
    if err != nil {
        return 0
    }
    defer rows.Close()
    userCount, _ := GetRowCount(rows)
    return userCount
}

func GetWorkspaceUserCountInPeriod(db *sql.DB, workspaceId int, billingStartDate time.Time, billingEndDate time.Time) int {
    rows, err := db.Query("SELECT COUNT(*) as count FROM workspaces_users WHERE workspace_id = ? AND ((activated_account_at BETWEEN ? AND ? AND status IN ('ACTIVE', 'TERMINATED')) OR status = 'ACTIVE')", workspaceId, billingStartDate, billingEndDate)
    if err != nil {
        return 0
    }
    defer rows.Close()
    userCount, _ := GetRowCount(rows)
    return userCount
}


func CreateInvoiceConfirmationNumber() (string, error) {
    b := make([]byte, 12)
    if _, err := rand.Read(b); err != nil {
        return "", err
    }
    return fmt.Sprintf("INV-%08X", b[:4]), nil
}

func CreateTaxMetadata(callTollsCosts, recordingCosts, faxCosts, membershipCosts, numberRentalCosts int64) string {
    taxMetadata := map[string]int64{
        "call_tolls_costs":    callTollsCosts,
        "recording_costs":     recordingCosts,
        "fax_costs":           faxCosts,
        "membership_costs":    membershipCosts,
        "number_rental_costs": numberRentalCosts,
    }
    b, _ := json.Marshal(taxMetadata)
    return string(b)
}

func CalculateInitialCharge(price float64, billingType string) (float64, time.Time) {
    now := time.Now()
    var nextAnchor time.Time
    
    if billingType == "MONTHLY" {
        // Next 1st of the month
        nextAnchor = time.Date(now.Year(), now.Month()+1, 1, 0, 0, 0, 0, now.Location())
    } else if billingType == "ANNUAL" {
        // Next Jan 1st
        nextAnchor = time.Date(now.Year()+1, 1, 1, 0, 0, 0, 0, now.Location())
    }

    // Days in current month or year
    currentPeriodStart := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, now.Location())
    if billingType == "ANNUAL" {
        currentPeriodStart = time.Date(now.Year(), 1, 1, 0, 0, 0, 0, now.Location())
    }

    totalDaysInPeriod := nextAnchor.Sub(currentPeriodStart).Hours() / 24
    daysRemaining := nextAnchor.Sub(now).Hours() / 24

    // Prorated Amount
    amount := (price / totalDaysInPeriod) * daysRemaining
    
    // Standard rounding for currency
    return math.Round(amount*100) / 100, nextAnchor
}

func CheckDeduplicationKey(db *sql.DB, key string) int {
    var count int
    err := db.QueryRow("SELECT COUNT(*) FROM billing_deduplication WHERE `key` = ?", key).Scan(&count)
    if err != nil {
        return 0
    }

    return count
}

func GetBillingFlow(customizations *helpers.CustomizationSettingsKV) string {
	var flow string = ""

	if interfacePtr, ok := customizations.Pairs["billing_flow"]; ok && interfacePtr != nil {
		if stringStruct, ok := (*interfacePtr).(*helpers.CustomizationStringValue); ok {
			flow = stringStruct.Value
		}
	}

	return flow
}

func GetGracePeriod(customizations *helpers.CustomizationSettingsKV) string {
	var flow string = ""

	if interfacePtr, ok := customizations.Pairs["grace_period_billing_days"]; ok && interfacePtr != nil {
		if stringStruct, ok := (*interfacePtr).(*helpers.CustomizationStringValue); ok {
			flow = stringStruct.Value
		}
	}

	return flow
}



func GetInvoiceDueInDays(customizations *helpers.CustomizationSettingsKV) int {
	invoiceDueDays := 7 // default

	if interfacePtr, ok := customizations.Pairs["invoice_due_in_days"]; ok && interfacePtr != nil {
		if stringStruct, ok := (*interfacePtr).(*helpers.CustomizationStringValue); ok {
			if v, err := strconv.Atoi(stringStruct.Value); err == nil {
				invoiceDueDays = v
			}
		}
	}

	return invoiceDueDays
}

// CalculateNextDate advances the billing date while clamping to the last day of short months
func CalculateNextDate(baseTime time.Time, cycle string, anchorDay int) time.Time {
	if cycle == "ANNUAL" {
		return baseTime.AddDate(1, 0, 0)
	}

	// Move directly to the next month, matching year and month properties
	nextMonth := baseTime.AddDate(0, 1, 0)
	year, month, _ := nextMonth.Date()

	// Find the true maximum number of days available in that target month
	// e.g., t.Date(year, month+1, 0) gives the last day of the current month
	lastDayOfTargetMonth := time.Date(year, month+1, 0, 0, 0, 0, 0, time.UTC).Day()

	targetDay := anchorDay
	if targetDay > lastDayOfTargetMonth {
		targetDay = lastDayOfTargetMonth // Clamped!
	}

	return time.Date(year, month, targetDay, 0, 0, 0, 0, time.UTC)
}

// GenerateInvoiceNumber generates a unique invoice number compliant with billing standards
// Format: INV-{workspaceID}-{randomSuffix}
// Example: INV-12345-ABC123
func GenerateInvoiceNumber(workspaceID, userID string) (string, error) {
	// Generate random suffix (6 alphanumeric characters for uniqueness)
	randomSuffix, err := generateRandomSuffix(6)
	if err != nil {
		return "", fmt.Errorf("failed to generate random suffix: %w", err)
	}

	// Construct invoice number following ISO invoice numbering standards
	invoiceNumber := fmt.Sprintf("INV-%s-%s",
		workspaceID,
		randomSuffix,
	)

	return invoiceNumber, nil
}

// generateRandomSuffix generates a random alphanumeric string of specified length
func generateRandomSuffix(length int) (string, error) {
	const charset = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZ"
	b := make([]byte, length)

	_, err := rand.Read(b)
	if err != nil {
		return "", err
	}

	for i := 0; i < length; i++ {
		b[i] = charset[b[i]%byte(len(charset))]
	}

	return string(b), nil
}

