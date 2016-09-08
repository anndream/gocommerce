package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/PuerkitoBio/goquery"
	"github.com/Sirupsen/logrus"
	"github.com/dgrijalva/jwt-go"
	"github.com/guregu/kami"
	"github.com/jinzhu/gorm"
	"github.com/mattes/vat"
	"github.com/netlify/gocommerce/models"
	"github.com/pborman/uuid"

	"golang.org/x/net/context"
)

type OrderLineItem struct {
	SKU      string `json:sku`
	Path     string `json:"path"`
	Quantity uint64 `json:"quantity"`
}

type OrderParams struct {
	SessionID string `json:"session_id"`

	Email string `json:"email"`

	ShippingAddressID string          `json:"shipping_address_id"`
	ShippingAddress   *models.Address `json:"shipping_address"`

	BillingAddressID string          `json:"billing_address_id"`
	BillingAddress   *models.Address `json:"billing_address"`

	VATNumber string `json:"vatnumber"`

	Data map[string]interface{} `json:"data"`

	LineItems []*OrderLineItem `json:"line_items"`

	Currency string `json:"currency"`
}

type verificationError struct {
	err   error
	mutex sync.Mutex
}

func (e *verificationError) setError(err error) {
	e.mutex.Lock()
	e.err = err
	e.mutex.Unlock()
}

func parseParams(query *gorm.DB, params url.Values) (*gorm.DB, error) {
	if values, exists := params["from"]; exists {
		date, err := time.Parse(time.RFC3339, values[0])
		if err != nil {
			return nil, fmt.Errorf("bad value for 'from' parameter: %s", err)
		}
		query = query.Where("created_at >= ?", date)
	}

	if values, exists := params["to"]; exists {
		date, err := time.Parse(time.RFC3339, values[0])
		if err != nil {
			return nil, fmt.Errorf("bad value for 'to' parameter: %s", err)
		}
		query = query.Where("created_at <= ?", date)
	}

	if values, exists := params["sort"]; exists {
		switch values[0] {
		case "desc":
			query = query.Order("created_at DESC")
		case "asc":
			query = query.Order("created_at ASC")
		default:
			return nil, fmt.Errorf("bad value for 'sort' parameter: only 'asc' or 'desc' are accepted")
		}
	}

	return query, nil
}

// OrderList can query based on
//  - orders since        &from=iso8601      - default = 0
//  - orders before       &to=iso8601        - default = now
//  - sort asc or desc    &sort=[asc | desc] - default = desc
func (a *API) OrderList(ctx context.Context, w http.ResponseWriter, r *http.Request) {
	log := Logger(ctx)
	var err error
	claims := Claims(ctx)
	if claims == nil {
		log.Info("Request with no claims made")
		UnauthorizedError(w, "Order History Requires Authentication")
		return
	}

	params := r.URL.Query()
	query := orderQuery(a.db)
	query, err = parseParams(query, params)
	if err != nil {
		log.WithError(err).Info("Bad query parameters in request")
		BadRequestError(w, "Bad parameters in query: "+err.Error())
		return
	}

	// handle the admin info
	id := claims.ID
	if values, exists := params["user_id"]; exists {
		if IsAdmin(ctx) {
			id = values[0]
			log.WithField("admin_id", claims.ID).Debugf("Making admin request for user %s by %s", id, claims.ID)
		} else {
			log.Warnf("Request for user id %s as user %s - but not an admin", values[0], id)
			BadRequestError(w, "Can't request user id if you're not that user, or an admin")
			return
		}
	}
	query = query.Where("user_id = ?", id)
	log.WithField("query_user_id", id).Debug("URL parsed and query perpared")

	var orders []models.Order
	result := query.Find(&orders)
	if result.Error != nil {
		log.WithError(err).Warn("Error while querying database")
		InternalServerError(w, fmt.Sprintf("Error during database query: %v", result.Error))
		return
	}

	log.WithField("order_count", len(orders)).Debugf("Successfully retrieved %d orders", len(orders))
	sendJSON(w, 200, orders)
}

// OrderView will request a specific order using the 'id' parameter.
// Only the owner of the order, an admin, or an anon order are allowed to be seen
func (a *API) OrderView(ctx context.Context, w http.ResponseWriter, r *http.Request) {
	log := Logger(ctx)
	claims := Claims(ctx)
	if claims == nil {
		log.Info("Request with no claims made")
		UnauthorizedError(w, "Order History Requires Authentication")
		return
	}

	id := kami.Param(ctx, "id")
	if id == "" {
		log.Warn("Request made with no id parameter")
		BadRequestError(w, "Must pass an id parameter")
		return
	}
	log = log.WithField("order_id", id)

	order := &models.Order{}
	if result := orderQuery(a.db).First(order, "id = ?", id); result.Error != nil {
		if result.RecordNotFound() {
			log.Debug("Requested record that doesn't exist")
			NotFoundError(w, "Order not found")
		} else {
			log.WithError(result.Error).Warnf("Error while querying database: %s", result.Error.Error())
			InternalServerError(w, "Error during database query")
		}
		return
	}

	if order.UserID == "" || (order.UserID == claims.ID) || IsAdmin(ctx) {
		log.Debugf("Successfully got order %s", order.ID)
		sendJSON(w, 200, order)
	} else {
		log.WithFields(logrus.Fields{
			"user_id":     claims.ID,
			"user_groups": claims.Groups,
		}).Warnf("Unauthorized access attempted for order %s by %s", order.ID, claims.ID)
		UnauthorizedError(w, "You don't have access to this order")
	}
}

// OrderCreate endpoint
func (a *API) OrderCreate(ctx context.Context, w http.ResponseWriter, r *http.Request) {
	params := &OrderParams{Currency: "USD"}
	jsonDecoder := json.NewDecoder(r.Body)
	err := jsonDecoder.Decode(params)
	if err != nil {
		BadRequestError(w, fmt.Sprintf("Could not read Order params: %v", err))
		return
	}

	token := getToken(ctx)

	order := models.NewOrder(params.SessionID, params.Email, params.Currency)

	tx := a.db.Begin()

	user := &models.User{}
	if httpError := a.setUserIDFromToken(tx, user, order, token); httpError != nil {
		sendJSON(w, httpError.Code, err)
		return
	}

	if httpError := a.createLineItems(ctx, tx, order, params.LineItems); httpError != nil {
		sendJSON(w, httpError.Code, httpError)
		return
	}

	shippingID, httpError := a.processAddress(tx, order, params.ShippingAddress, params.ShippingAddressID)
	if httpError != nil {
		sendJSON(w, httpError.Code, httpError)
		return
	}
	if shippingID == "" {
		BadRequestError(w, "Shipping Address Required")
		return
	}
	order.ShippingAddressID = shippingID

	billingID, httpError := a.processAddress(tx, order, params.BillingAddress, params.BillingAddressID)
	if httpError != nil {
		sendJSON(w, httpError.Code, httpError)
		return
	}
	if billingID != "" {
		order.BillingAddressID = billingID
	} else {
		order.BillingAddressID = shippingID
	}

	if params.VATNumber != "" {
		valid, err := vat.IsValidVAT(params.VATNumber)
		if err != nil {
			tx.Rollback()
			InternalServerError(w, fmt.Sprintf("Error verifying VAT number %v", err))
			return
		}
		if !valid {
			tx.Rollback()
			BadRequestError(w, fmt.Sprintf("Vat number %v is not valid", order.VATNumber))
			return
		}
		order.VATNumber = params.VATNumber
	}

	if params.Data != nil {
		if err := order.UpdateOrderData(tx, &params.Data); err != nil {
			tx.Rollback()
			BadRequestError(w, fmt.Sprintf("Bad order metadata %v", err))
			return
		}
	}

	tx.Create(order)
	tx.Commit()

	sendJSON(w, 200, order)
}

func (a *API) setUserIDFromToken(tx *gorm.DB, user *models.User, order *models.Order, token *jwt.Token) *HTTPError {
	if token != nil {
		claims := token.Claims.(*JWTClaims)
		if claims.ID == "" {
			tx.Rollback()
			return &HTTPError{Code: 400, Message: fmt.Sprintf("Token had an invalid ID: %v", claims.ID)}
		}
		order.UserID = claims.ID
		if result := tx.First(user, "id = ?", claims.ID); result.Error != nil {
			if result.RecordNotFound() {
				user.ID = claims.ID
				if claims.Email != "" {
					user.Email = claims.Email
				} else {
					order.Email = user.Email
				}
				tx.Create(user)
			} else {
				tx.Rollback()
				return &HTTPError{Code: 500, Message: fmt.Sprintf("Token had an invalid ID: %v", result.Error)}
			}
		}
	}
	return nil
}

func (a *API) createLineItems(ctx context.Context, tx *gorm.DB, order *models.Order, items []*OrderLineItem) *HTTPError {
	sem := make(chan int, MaxConcurrentLookups)
	var wg sync.WaitGroup
	sharedErr := verificationError{}
	for _, item := range items {
		lineItem := &models.LineItem{SKU: item.SKU, Quantity: item.Quantity, Path: item.Path, OrderID: order.ID}
		order.LineItems = append(order.LineItems, lineItem)
		sem <- 1
		wg.Add(1)
		go func(item *models.LineItem) {
			defer func() {
				wg.Done()
				<-sem
			}()
			// Stop doing any work if there's already an error
			if sharedErr.err != nil {
				return
			}

			if err := a.processLineItem(ctx, order, item); err != nil {
				sharedErr.setError(err)
			}
		}(lineItem)
	}
	wg.Wait()

	if sharedErr.err != nil {
		tx.Rollback()
		return &HTTPError{Code: 500, Message: fmt.Sprintf("Error processing line item: %v", sharedErr.err)}
	}

	for _, item := range order.LineItems {
		order.SubTotal = order.SubTotal + item.Price*item.Quantity
		if err := tx.Create(&item).Error; err != nil {
			tx.Rollback()
			return &HTTPError{Code: 500, Message: fmt.Sprintf("Error creating line item: %v", err)}
		}
	}

	return nil
}

func (a *API) processAddress(tx *gorm.DB, order *models.Order, address *models.Address, id string) (string, *HTTPError) {
	if id != "" {
		if result := tx.First(address, "id = ?", id); result.Error != nil {
			tx.Rollback()
			return "", &HTTPError{Code: 400, Message: fmt.Sprintf("Bad address id: %v", id)}
		}
		if address.UserID == "" {
			address.UserID = order.UserID
			tx.Save(address)
		} else if order.UserID != address.UserID {
			tx.Rollback()
			return "", &HTTPError{Code: 400, Message: fmt.Sprintf("Bad address id: %v", id)}
		}
	} else {
		if address == nil {
			return "", nil
		} else if address.Valid() {
			address.ID = uuid.NewRandom().String()
			tx.Create(address)
		} else {
			tx.Rollback()
			return "", &HTTPError{Code: 400, Message: "Failed to validate address"}
		}
	}
	return address.ID, nil
}

func (a *API) processLineItem(ctx context.Context, order *models.Order, item *models.LineItem) error {
	config := getConfig(ctx)
	resp, err := a.httpClient.Get(config.SiteURL + item.Path)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	doc, err := goquery.NewDocumentFromResponse(resp)
	if err != nil {
		return err
	}

	metaTag := doc.Find("#gocommerce-product").First()
	if metaTag.Length() == 0 {
		return fmt.Errorf("No script tag with id gocommerce-product tag found for '%v'", item.Title)
	}
	meta := &models.LineItemMetadata{}
	err = json.Unmarshal([]byte(metaTag.Text()), meta)
	if err != nil {
		return err
	}

	return item.Process(order, meta)
}

func orderQuery(db *gorm.DB) *gorm.DB {
	return db.Preload("LineItems").Preload("ShippingAddress").Preload("BillingAddress")
}
