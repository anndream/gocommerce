package calculator

import (
	"math"
	"strconv"

	"github.com/netlify/gocommerce/claims"
)

// Price represents the total price of all line items.
type Price struct {
	Items []ItemPrice

	Subtotal uint64
	Discount uint64
	Taxes    uint64
	Total    uint64
}

// ItemPrice is the price of a single line item.
type ItemPrice struct {
	Quantity uint64

	Subtotal uint64
	Discount uint64
	Taxes    uint64
	Total    uint64
}

// Settings represent the site-wide settings for price calculation.
type Settings struct {
	PricesIncludeTaxes bool              `json:"prices_include_taxes"`
	Taxes              []*Tax            `json:"taxes"`
	MemberDiscounts    []*MemberDiscount `json:"member_discounts"`
}

// Tax represents a tax, potentially specific to countries and product types.
type Tax struct {
	Percentage   uint64   `json:"percentage"`
	ProductTypes []string `json:"product_types"`
	Countries    []string `json:"countries"`
}

type taxAmount struct {
	price      uint64
	percentage uint64
}

// FixedMemberDiscount represents a fixed discount given to members.
type FixedMemberDiscount struct {
	Amount   string `json:"amount"`
	Currency string `json:"currency"`
}

// MemberDiscount represents a discount given to members, either fixed
// or a percentage.
type MemberDiscount struct {
	Claims       map[string]string      `json:"claims"`
	Percentage   uint64                 `json:"percentage"`
	FixedAmount  []*FixedMemberDiscount `json:"fixed"`
	ProductTypes []string               `json:"product_types"`
	Products     []string               `json:"products"`
}

// ValidForType returns whether a member discount is valid for a product type.
func (d *MemberDiscount) ValidForType(productType string) bool {
	if d.ProductTypes == nil || len(d.ProductTypes) == 0 {
		return true
	}
	for _, validType := range d.ProductTypes {
		if validType == productType {
			return true
		}
	}
	return false
}

// ValidForProduct returns whether a member discount is valid for a product sku
func (d *MemberDiscount) ValidForProduct(productSku string) bool {
	if d.Products == nil || len(d.Products) == 0 {
		return true
	}
	for _, validSku := range d.Products {
		if validSku == productSku {
			return true
		}
	}
	return false
}

// Item is the interface for a single line item needed to do price calculation.
type Item interface {
	ProductSku() string
	PriceInLowestUnit() uint64
	ProductType() string
	FixedVAT() uint64
	TaxableItems() []Item
	GetQuantity() uint64
}

// Coupon is the interface for a coupon needed to do price calculation.
type Coupon interface {
	ValidForType(string) bool
	ValidForPrice(string, uint64) bool
	ValidForProduct(string) bool
	PercentageDiscount() uint64
	FixedDiscount(string) uint64
}

// FixedDiscount returns what the fixed discount amount is for a particular currency.
func (d *MemberDiscount) FixedDiscount(currency string) uint64 {
	if d.FixedAmount != nil {
		for _, discount := range d.FixedAmount {
			if discount.Currency == currency {
				amount, _ := strconv.ParseFloat(discount.Amount, 64)
				return rint(amount * 100)
			}
		}
	}

	return 0
}

// AppliesTo determines if the tax applies to the country AND product type provided.
func (t *Tax) AppliesTo(country, productType string) bool {
	applies := true
	if t.ProductTypes != nil && len(t.ProductTypes) > 0 {
		applies = false
		for _, t := range t.ProductTypes {
			if t == productType {
				applies = true
				break
			}
		}
	}
	if !applies {
		return false
	}
	if t.Countries != nil && len(t.Countries) > 0 {
		applies = false
		for _, c := range t.Countries {
			if c == country {
				applies = true
				break
			}
		}
	}
	return applies
}

// CalculatePrice will calculate the final total price. It takes into account
// currency, country, coupons, and discounts.
func CalculatePrice(settings *Settings, jwtClaims map[string]interface{}, country, currency string, coupon Coupon, items []Item) Price {
	price := Price{}
	includeTaxes := settings != nil && settings.PricesIncludeTaxes
	for _, item := range items {
		itemPrice := ItemPrice{Quantity: item.GetQuantity()}
		itemPrice.Subtotal = item.PriceInLowestUnit()

		taxAmounts := []taxAmount{}
		if item.FixedVAT() != 0 {
			taxAmounts = append(taxAmounts, taxAmount{price: itemPrice.Subtotal, percentage: item.FixedVAT()})
		} else if settings != nil && item.TaxableItems() != nil && len(item.TaxableItems()) > 0 {
			for _, item := range item.TaxableItems() {
				amount := taxAmount{price: item.PriceInLowestUnit()}
				for _, t := range settings.Taxes {
					if t.AppliesTo(country, item.ProductType()) {
						amount.percentage = t.Percentage
						break
					}
				}
				taxAmounts = append(taxAmounts, amount)
			}
		} else if settings != nil {
			for _, t := range settings.Taxes {
				if t.AppliesTo(country, item.ProductType()) {
					taxAmounts = append(taxAmounts, taxAmount{price: itemPrice.Subtotal, percentage: t.Percentage})
					break
				}
			}
		}

		if len(taxAmounts) != 0 {
			if includeTaxes {
				itemPrice.Subtotal = 0
			}
			for _, tax := range taxAmounts {
				if includeTaxes {
					tax.price = rint(float64(tax.price) / (100 + float64(tax.percentage)) * 100)
					itemPrice.Subtotal += tax.price
				}
				itemPrice.Taxes += rint(float64(tax.price) * float64(tax.percentage) / 100)
			}
		}
		if coupon != nil && coupon.ValidForType(item.ProductType()) && coupon.ValidForProduct(item.ProductSku()) {
			itemPrice.Discount = calculateDiscount(itemPrice.Subtotal, itemPrice.Taxes, coupon.PercentageDiscount(), coupon.FixedDiscount(currency), includeTaxes)
		}
		if settings != nil && settings.MemberDiscounts != nil {
			for _, discount := range settings.MemberDiscounts {
				if jwtClaims != nil && claims.HasClaims(jwtClaims, discount.Claims) && discount.ValidForType(item.ProductType()) {
					itemPrice.Discount += calculateDiscount(itemPrice.Subtotal, itemPrice.Taxes, discount.Percentage, discount.FixedDiscount(currency), includeTaxes)
				}
			}
		}

		itemPrice.Total = itemPrice.Subtotal - itemPrice.Discount + itemPrice.Taxes
		if itemPrice.Total < 0 {
			itemPrice.Total = 0
		}

		price.Items = append(price.Items, itemPrice)

		price.Subtotal += (itemPrice.Subtotal * itemPrice.Quantity)
		price.Discount += (itemPrice.Discount * itemPrice.Quantity)
		price.Taxes += (itemPrice.Taxes * itemPrice.Quantity)
		price.Total += (itemPrice.Total * itemPrice.Quantity)
	}

	price.Total = price.Subtotal - price.Discount + price.Taxes

	return price
}

func calculateDiscount(amountToDiscount, taxes, percentage, fixed uint64, includeTaxes bool) uint64 {
	if includeTaxes {
		amountToDiscount += taxes
	}
	var discount uint64
	if percentage > 0 {
		discount = rint(float64(amountToDiscount) * float64(percentage) / 100)
	}
	discount += fixed

	if discount > amountToDiscount {
		return amountToDiscount
	}
	return discount
}

// Nopes - no `round` method in go
// See https://gist.github.com/siddontang/1806573b9a8574989ccb
func rint(x float64) uint64 {
	v, frac := math.Modf(x)
	if x > 0.0 {
		if frac > 0.5 || (frac == 0.5 && uint64(v)%2 != 0) {
			v += 1.0
		}
	} else {
		if frac < -0.5 || (frac == -0.5 && uint64(v)%2 != 0) {
			v -= 1.0
		}
	}

	return uint64(v)
}
