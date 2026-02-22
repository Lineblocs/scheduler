package cmd

import (
	"errors"
	"math"
	"regexp"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	helpers "github.com/Lineblocs/go-helpers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"lineblocs.com/scheduler/mocks"
)

func testAnnualServicePlans() []helpers.ServicePlan {
	return []helpers.ServicePlan{
		{
			MinutesPerMonth:          200.0,
			BaseCosts:                24.99,
			ImIntegrations:           true,
			Name:                     "starter",
			ProductivityIntegrations: true,
			RecordingSpace:           1024.0,
		},
	}
}

func TestAnnualBilling(t *testing.T) {
	t.Parallel()
	helpers.InitLogrus("file")

	testWorkspace := &helpers.Workspace{
		Id:        1,
		CreatorId: 101,
		Plan:      "starter",
	}

	testUser := &helpers.User{
		Id: 101,
	}

	t.Run("Should fail AnnualBilling job due unable to get payment gateway", func(t *testing.T) {
		t.Parallel()

		mockWorkspace := &mocks.WorkspaceRepository{}
		mockPayment := &mocks.PaymentRepository{}

		db, mockSql, err := sqlmock.New()
		assert.NoError(t, err)

		defer db.Close()

		error := errors.New("failed to get payment_gateway")
		// Mock expectations for GetBillingParams
		mockSql.ExpectQuery("SELECT payment_gateway FROM customizations").
			WillReturnError(error)

		job := NewAnnualBillingJob(db, mockWorkspace, mockPayment)
		err = job.AnnualBilling()
		assert.Error(t, err)
		assert.Equal(t, error, err)

		err = mockSql.ExpectationsWereMet()
		assert.NoError(t, err)
	})

	t.Run("Should fail AnnualBilling job due unable to get workspace information", func(t *testing.T) {
		t.Parallel()

		mockWorkspace := &mocks.WorkspaceRepository{}
		mockPayment := &mocks.PaymentRepository{}

		db, mockSql, err := sqlmock.New()
		assert.NoError(t, err)

		defer db.Close()

		// Mock expectations for GetBillingParams
		mockSql.ExpectQuery("SELECT payment_gateway FROM customizations").
			WillReturnRows(sqlmock.NewRows([]string{"payment_gateway"}).
				AddRow("stripe"))

		mockSql.ExpectQuery("SELECT stripe_private_key FROM api_credentials").
			WillReturnRows(sqlmock.NewRows([]string{"stripe_private_key"}).
				AddRow("test_stripe_key"))

		error := errors.New("failed to get workspaces")
		// Mock expectations for the workspaces query
		mockSql.ExpectQuery("SELECT id, creator_id FROM workspaces WHERE plan_term = 'annual'").
			WillReturnError(error)

		job := NewAnnualBillingJob(db, mockWorkspace, mockPayment)
		err = job.AnnualBilling()
		assert.Error(t, err)
		assert.Equal(t, error, err)

		err = mockSql.ExpectationsWereMet()
		assert.NoError(t, err)
	})

	t.Run("Should fail AnnualBilling job due unable to get workspace information", func(t *testing.T) {
		t.Parallel()

		mockWorkspace := &mocks.WorkspaceRepository{}
		mockPayment := &mocks.PaymentRepository{}

		db, mockSql, err := sqlmock.New()
		assert.NoError(t, err)

		defer db.Close()

		// Mock expectations for GetBillingParams
		mockSql.ExpectQuery("SELECT payment_gateway FROM customizations").
			WillReturnRows(sqlmock.NewRows([]string{"payment_gateway"}).
				AddRow("stripe"))

		mockSql.ExpectQuery("SELECT stripe_private_key FROM api_credentials").
			WillReturnRows(sqlmock.NewRows([]string{"stripe_private_key"}).
				AddRow("test_stripe_key"))

		error := errors.New("failed to get workspaces")
		// Mock expectations for the workspaces query
		mockSql.ExpectQuery("SELECT id, creator_id FROM workspaces WHERE plan_term = 'annual'").
			WillReturnError(error)

		job := NewAnnualBillingJob(db, mockWorkspace, mockPayment)
		err = job.AnnualBilling()
		assert.Error(t, err)
		assert.Equal(t, error, err)

		err = mockSql.ExpectationsWereMet()
		assert.NoError(t, err)
	})

	t.Run("Should finish AnnualBilling job without processing due unable to get user from db", func(t *testing.T) {
		t.Parallel()

		mockWorkspace := &mocks.WorkspaceRepository{}
		mockPayment := &mocks.PaymentRepository{}

		mockWorkspace.EXPECT().GetWorkspaceFromDB(mock.Anything).Return(testWorkspace, nil)
		mockWorkspace.EXPECT().GetUserFromDB(mock.Anything).Return(nil, errors.New("failed to get user"))

		mockPayment.EXPECT().GetServicePlans().Return(testAnnualServicePlans(), nil)

		db, mockSql, err := sqlmock.New()
		assert.NoError(t, err)

		defer db.Close()

		// Mock expectations for GetBillingParams
		mockSql.ExpectQuery("SELECT payment_gateway FROM customizations").
			WillReturnRows(sqlmock.NewRows([]string{"payment_gateway"}).
				AddRow("stripe"))

		mockSql.ExpectQuery("SELECT stripe_private_key FROM api_credentials").
			WillReturnRows(sqlmock.NewRows([]string{"stripe_private_key"}).
				AddRow("test_stripe_key"))

		// Mock expectations for the workspaces query
		mockSql.ExpectQuery("SELECT id, creator_id FROM workspaces WHERE plan_term = 'annual'").
			WillReturnRows(sqlmock.NewRows([]string{"id", "creator_id"}).
				AddRow(testWorkspace.Id, testWorkspace.CreatorId))

		job := NewAnnualBillingJob(db, mockWorkspace, mockPayment)
		err = job.AnnualBilling()
		assert.NoError(t, err)

		err = mockSql.ExpectationsWereMet()
		assert.NoError(t, err)
	})

	t.Run("Should finish AnnualBilling job without processing any payment due db issues", func(t *testing.T) {
		t.Parallel()

		worksSpaceUsers := 3

		mockWorkspace := &mocks.WorkspaceRepository{}
		mockPayment := &mocks.PaymentRepository{}

		//Create Starter Workspace
		mockWorkspace.EXPECT().GetWorkspaceFromDB(mock.Anything).Return(testWorkspace, nil)
		mockWorkspace.EXPECT().GetUserFromDB(mock.Anything).Return(testUser, nil)

		mockPayment.EXPECT().ChargeCustomer(mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(nil)
		mockPayment.EXPECT().GetServicePlans().Return(testAnnualServicePlans(), nil)

		db, mockSql, err := sqlmock.New()
		assert.NoError(t, err)

		defer db.Close()

		// Mock expectations for GetBillingParams
		mockSql.ExpectQuery("SELECT payment_gateway FROM customizations").
			WillReturnRows(sqlmock.NewRows([]string{"payment_gateway"}).
				AddRow("stripe"))

		mockSql.ExpectQuery("SELECT stripe_private_key FROM api_credentials").
			WillReturnRows(sqlmock.NewRows([]string{"stripe_private_key"}).
				AddRow("test_stripe_key"))

		// Mock expectations for the workspaces query
		mockSql.ExpectQuery("SELECT id, creator_id FROM workspaces WHERE plan_term = 'annual'").
			WillReturnRows(sqlmock.NewRows([]string{"id", "creator_id"}).
				AddRow(testWorkspace.Id, testWorkspace.CreatorId))

		// Mock expectations for user count query
		userCountQuery := "SELECT COUNT(*) as count FROM  workspaces_users WHERE workspace_id = ?"
		mockSql.ExpectQuery(regexp.QuoteMeta(userCountQuery)).
			WithArgs(testWorkspace.Id).
			WillReturnRows(sqlmock.NewRows([]string{"count"}).
				AddRow(worksSpaceUsers))

		// Mock expectations for the INSERT into users_invoices
		membershipCosts := float64(0) * float64(worksSpaceUsers)
		totalCostsCents := int(math.Ceil(membershipCosts))
		invoiceStatus := "INCOMPLETE"
		regularCostsCents := 0

		// Mock expectations for the INSERT into users_invoices
		sqlQuery := "INSERT INTO users_invoices (`cents`, `membership_costs`, `status`, `user_id`, `workspace_id`, `created_at`, `updated_at`) VALUES ( ?, ?, ?, ?, ?, ?, ?)"
		mockSql.ExpectPrepare(regexp.QuoteMeta(sqlQuery)).
			ExpectExec().
			WithArgs(regularCostsCents, totalCostsCents, invoiceStatus, testWorkspace.CreatorId, testWorkspace.Id, sqlmock.AnyArg(), sqlmock.AnyArg()).
			WillReturnResult(sqlmock.NewResult(1, 1))

		// Mock expectations for the LastInsertId
		sqlInsertId := "UPDATE users_invoices SET status = 'COMPLETE', source ='CARD', cents_collected = ?, confirmation_number = ? WHERE id = ?"
		escapedInsertId := regexp.QuoteMeta(sqlInsertId)
		mockSql.ExpectPrepare(escapedInsertId).
			ExpectExec().
			WithArgs(totalCostsCents, sqlmock.AnyArg(), 1).
			WillReturnError(errors.New("failed to update users_invoices"))

		job := NewAnnualBillingJob(db, mockWorkspace, mockPayment)
		err = job.AnnualBilling()
		assert.NoError(t, err)

		err = mockSql.ExpectationsWereMet()
		assert.NoError(t, err)
	})

	t.Run("Should finish AnnualBilling job without processing a payment due unable to charge customer", func(t *testing.T) {
		t.Parallel()

		worksSpaceUsers := 3

		mockWorkspace := &mocks.WorkspaceRepository{}
		mockPayment := &mocks.PaymentRepository{}

		mockWorkspace.EXPECT().GetWorkspaceFromDB(mock.Anything).Return(testWorkspace, nil)
		mockWorkspace.EXPECT().GetUserFromDB(mock.Anything).Return(testUser, nil)

		mockPayment.EXPECT().ChargeCustomer(mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(errors.New("unable to charge customer"))
		mockPayment.EXPECT().GetServicePlans().Return(testAnnualServicePlans(), nil)

		db, mockSql, err := sqlmock.New()
		assert.NoError(t, err)

		defer db.Close()

		// Mock expectations for GetBillingParams
		mockSql.ExpectQuery("SELECT payment_gateway FROM customizations").
			WillReturnRows(sqlmock.NewRows([]string{"payment_gateway"}).
				AddRow("stripe"))

		mockSql.ExpectQuery("SELECT stripe_private_key FROM api_credentials").
			WillReturnRows(sqlmock.NewRows([]string{"stripe_private_key"}).
				AddRow("test_stripe_key"))

		// Mock expectations for the workspaces query
		mockSql.ExpectQuery("SELECT id, creator_id FROM workspaces WHERE plan_term = 'annual'").
			WillReturnRows(sqlmock.NewRows([]string{"id", "creator_id"}).
				AddRow(1, testWorkspace.CreatorId))

		// Mock expectations for user count query
		userCountQuery := "SELECT COUNT(*) as count FROM  workspaces_users WHERE workspace_id = ?"
		mockSql.ExpectQuery(regexp.QuoteMeta(userCountQuery)).
			WithArgs(testWorkspace.Id).
			WillReturnRows(sqlmock.NewRows([]string{"count"}).
				AddRow(worksSpaceUsers))

		// Mock expectations for the INSERT into users_invoices
		membershipCosts := float64(0) * float64(worksSpaceUsers)
		totalCostsCents := int(math.Ceil(membershipCosts))
		invoiceStatus := "INCOMPLETE"
		regularCostsCents := 0

		// Mock expectations for the INSERT into users_invoices
		sqlQuery := "INSERT INTO users_invoices (`cents`, `membership_costs`, `status`, `user_id`, `workspace_id`, `created_at`, `updated_at`) VALUES ( ?, ?, ?, ?, ?, ?, ?)"
		mockSql.ExpectPrepare(regexp.QuoteMeta(sqlQuery)).
			ExpectExec().
			WithArgs(regularCostsCents, totalCostsCents, invoiceStatus, testWorkspace.CreatorId, testWorkspace.Id, sqlmock.AnyArg(), sqlmock.AnyArg()).
			WillReturnResult(sqlmock.NewResult(1, 1))

		// Mock expectations for the LastInsertId
		sqlInsertId := "UPDATE users_invoices SET status = 'INCOMPLETE', source = 'CARD', cents_collected = 0.0 WHERE id = ?"
		escapedInsertId := regexp.QuoteMeta(sqlInsertId)
		mockSql.ExpectPrepare(escapedInsertId).
			ExpectExec().
			WithArgs(1).
			WillReturnResult(sqlmock.NewResult(1, 1))

		job := NewAnnualBillingJob(db, mockWorkspace, mockPayment)
		err = job.AnnualBilling()
		assert.NoError(t, err)

		err = mockSql.ExpectationsWereMet()
		assert.NoError(t, err)
	})

	t.Run("Should finish AnnualBilling job without any issues", func(t *testing.T) {
		t.Parallel()

		worksSpaceUsers := 3

		mockWorkspace := &mocks.WorkspaceRepository{}
		mockPayment := &mocks.PaymentRepository{}

		mockWorkspace.EXPECT().GetWorkspaceFromDB(mock.Anything).Return(testWorkspace, nil)
		mockWorkspace.EXPECT().GetUserFromDB(mock.Anything).Return(testUser, nil)

		mockPayment.EXPECT().ChargeCustomer(mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(nil)
		mockPayment.EXPECT().GetServicePlans().Return(testAnnualServicePlans(), nil)

		db, mockSql, err := sqlmock.New()
		assert.NoError(t, err)

		defer db.Close()

		// Mock expectations for GetBillingParams
		mockSql.ExpectQuery("SELECT payment_gateway FROM customizations").
			WillReturnRows(sqlmock.NewRows([]string{"payment_gateway"}).
				AddRow("stripe"))

		mockSql.ExpectQuery("SELECT stripe_private_key FROM api_credentials").
			WillReturnRows(sqlmock.NewRows([]string{"stripe_private_key"}).
				AddRow("test_stripe_key"))

		// Mock expectations for the workspaces query
		mockSql.ExpectQuery("SELECT id, creator_id FROM workspaces WHERE plan_term = 'annual'").
			WillReturnRows(sqlmock.NewRows([]string{"id", "creator_id"}).
				AddRow(testWorkspace.Id, testWorkspace.CreatorId))

		// Mock expectations for user count query
		userCountQuery := "SELECT COUNT(*) as count FROM  workspaces_users WHERE workspace_id = ?"
		mockSql.ExpectQuery(regexp.QuoteMeta(userCountQuery)).
			WithArgs(testWorkspace.Id).
			WillReturnRows(sqlmock.NewRows([]string{"count"}).
				AddRow(worksSpaceUsers))

		// Mock expectations for the INSERT into users_invoices
		membershipCosts := float64(0) * float64(worksSpaceUsers)
		totalCostsCents := int(math.Ceil(membershipCosts))
		invoiceStatus := "INCOMPLETE"
		regularCostsCents := 0

		// Mock expectations for the INSERT into users_invoices
		sqlQuery := "INSERT INTO users_invoices (`cents`, `membership_costs`, `status`, `user_id`, `workspace_id`, `created_at`, `updated_at`) VALUES ( ?, ?, ?, ?, ?, ?, ?)"
		mockSql.ExpectPrepare(regexp.QuoteMeta(sqlQuery)).
			ExpectExec().
			WithArgs(regularCostsCents, totalCostsCents, invoiceStatus, testWorkspace.CreatorId, testWorkspace.Id, sqlmock.AnyArg(), sqlmock.AnyArg()).
			WillReturnResult(sqlmock.NewResult(1, 1))

		// Mock expectations for the LastInsertId
		sqlInsertId := "UPDATE users_invoices SET status = 'COMPLETE', source ='CARD', cents_collected = ?, confirmation_number = ? WHERE id = ?"
		escapedInsertId := regexp.QuoteMeta(sqlInsertId)
		mockSql.ExpectPrepare(escapedInsertId).
			ExpectExec().
			WithArgs(totalCostsCents, sqlmock.AnyArg(), 1).
			WillReturnResult(sqlmock.NewResult(1, 1))

		job := NewAnnualBillingJob(db, mockWorkspace, mockPayment)
		err = job.AnnualBilling()
		assert.NoError(t, err)

		err = mockSql.ExpectationsWereMet()
		assert.NoError(t, err)
	})
}
