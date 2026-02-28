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

func CreateMonthlyNumberRentalDebit(db *sql.DB, workspaceId int, userId int, start time.Time) (int, int) {
    var didId int
    var monthlyCosts int
    results1, err := db.Query("SELECT id, monthly_cost FROM did_numbers WHERE workspace_id = ?", workspaceId)
    if err != nil {
        return 0, 0
    }
    defer results1.Close()
    for results1.Next() {
        results1.Scan(&didId, &monthlyCosts)
        stmt, _ := db.Prepare("INSERT INTO users_debits (`source`, `status`, `cents`, `module_id`, `user_id`, `workspace_id`, `created_at`) VALUES (?, ?, ?, ?, ?, ?, ?)")
        defer stmt.Close()
        _, _ = stmt.Exec("NUMBER_RENTAL", "INCOMPLETE", monthlyCosts, didId, userId, workspaceId, start)
    }
    return didId, monthlyCosts
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