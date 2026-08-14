package purchases

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"strings"
	"time"

	"launcher-backend/internal/models"
	"launcher-backend/internal/repo"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const PageSize = 50
const deliveryLease = time.Minute

var nickPattern = regexp.MustCompile(`^[A-Za-z0-9_]{3,16}$`)
var deliveryItemPattern = regexp.MustCompile(`^[a-z0-9_.-]+:[a-z0-9_./-]+$`)
var deliveryRoles = map[string]struct{}{"special_forces": {}, "uav_operator": {}, "ew_specialist": {}}
var ErrPaymentConflict = errors.New("payment already belongs to another order")
var ErrDeliveryInFlight = errors.New("delivery is being applied by the game server")

type ValidationError struct{ Message string }

func (e ValidationError) Error() string { return e.Message }

type CreateInput struct {
	OrderID    string
	Nick       string
	Items      []models.OrderItem
	Total      int64
	YooKassaID string
	PaidAt     time.Time
}

type ListOptions struct {
	Status string
	Query  string
	From   string
	To     string
	Page   int
}

type OrderPage struct {
	Items    []models.Order `json:"items"`
	Total    int64          `json:"total"`
	Page     int            `json:"page"`
	PageSize int            `json:"pageSize"`
}

type TopItem struct {
	ID      int64  `json:"id"`
	Name    string `json:"name"`
	Qty     int    `json:"qty"`
	Revenue int64  `json:"revenue"`
}

type Stats struct {
	Revenue int64     `json:"revenue"`
	Orders  int64     `json:"orders"`
	Pending int64     `json:"pending"`
	Top     []TopItem `json:"topItems"`
}

func validDelivery(spec models.DeliverySpec) bool {
	switch spec.Type {
	case models.DeliveryTypeNone:
		return true
	case models.DeliveryTypeRole:
		_, ok := deliveryRoles[spec.RoleID]
		return ok
	case models.DeliveryTypeItem:
		return deliveryItemPattern.MatchString(spec.ItemID) && spec.Count >= 1 && spec.Count <= 100
	default:
		return false
	}
}

type Service struct{ db *gorm.DB }

func NewService(db *gorm.DB) Service { return Service{db: db} }

func validateCreate(input CreateInput) error {
	parsed, err := uuid.Parse(input.OrderID)
	if err != nil || parsed.String() != strings.ToLower(input.OrderID) {
		return ValidationError{Message: "Некорректный номер заказа"}
	}
	if strings.TrimSpace(input.YooKassaID) == "" || len(input.YooKassaID) > 128 {
		return ValidationError{Message: "Некорректный id платежа"}
	}
	if !nickPattern.MatchString(input.Nick) {
		return ValidationError{Message: "Некорректный ник"}
	}
	if len(input.Items) == 0 || len(input.Items) > 100 {
		return ValidationError{Message: "Некорректный состав заказа"}
	}
	var calculated int64
	for _, item := range input.Items {
		if item.ID <= 0 || strings.TrimSpace(item.Name) == "" || len(item.Name) > 300 ||
			item.Qty < 1 || item.Qty > 10_000 || item.Price < 1 || item.Price > 100_000_000 ||
			!validDelivery(item.Delivery) {
			return ValidationError{Message: "Некорректный товар в заказе"}
		}
		line := item.Price * int64(item.Qty)
		if line/item.Price != int64(item.Qty) || calculated > 100_000_000-line {
			return ValidationError{Message: "Слишком большая сумма заказа"}
		}
		calculated += line
	}
	if input.Total <= 0 || input.Total != calculated {
		return ValidationError{Message: "Сумма заказа не совпадает с товарами"}
	}
	if input.PaidAt.IsZero() {
		return ValidationError{Message: "Не указано время оплаты"}
	}
	return nil
}

func (s Service) Create(ctx context.Context, input CreateInput) (models.Order, bool, error) {
	if err := validateCreate(input); err != nil {
		return models.Order{}, false, err
	}
	order := models.Order{
		OrderID: input.OrderID, Nick: input.Nick, Items: input.Items, Total: input.Total,
		YooKassaID: input.YooKassaID, Status: models.OrderStatusPaid, PaidAt: input.PaidAt.UTC(),
	}
	created := false
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		result := tx.Clauses(clause.OnConflict{
			Columns: []clause.Column{{Name: "order_id"}}, DoNothing: true,
		}).Omit("Deliveries").Create(&order)
		if result.Error != nil {
			return result.Error
		}
		created = result.RowsAffected == 1
		if !created {
			if err := tx.Preload("Deliveries").First(&order, "order_id = ?", input.OrderID).Error; err != nil {
				return err
			}
			if order.YooKassaID != input.YooKassaID || order.Nick != input.Nick || order.Total != input.Total ||
				!order.PaidAt.Truncate(time.Microsecond).Equal(input.PaidAt.UTC().Truncate(time.Microsecond)) ||
				!reflect.DeepEqual(order.Items, input.Items) {
				return ErrPaymentConflict
			}
			return nil
		}

		deliveries := make([]models.Delivery, 0, len(input.Items))
		for index, item := range input.Items {
			spec := item.Delivery
			if spec.Type == models.DeliveryTypeNone {
				continue
			}
			if spec.Type == models.DeliveryTypeItem {
				remaining := int64(spec.Count) * int64(item.Qty)
				for part := 0; remaining > 0; part++ {
					portion := min(remaining, int64(10_000))
					partSpec := spec
					partSpec.Count = int(portion)
					deliveries = append(deliveries, models.Delivery{
						OrderID: order.OrderID, ItemIndex: index, PartIndex: part,
						ShopItemID: item.ID, ItemName: item.Name, Nick: order.Nick,
						Delivery: partSpec, Status: models.DeliveryStatusPending,
					})
					remaining -= portion
				}
				continue
			}
			deliveries = append(deliveries, models.Delivery{
				OrderID: order.OrderID, ItemIndex: index, ShopItemID: item.ID, ItemName: item.Name,
				Nick: order.Nick, Delivery: spec, Status: models.DeliveryStatusPending,
			})
		}
		if len(deliveries) > 0 {
			if err := tx.Create(&deliveries).Error; err != nil {
				return err
			}
			order.Deliveries = deliveries
		}
		return nil
	})
	if err != nil {
		var existing models.Order
		if err := s.db.WithContext(ctx).First(&existing, "yoo_kassa_id = ?", input.YooKassaID).Error; err == nil {
			return models.Order{}, false, ErrPaymentConflict
		}
		return models.Order{}, false, err
	}
	return order, created, nil
}

func applyPeriod(query *gorm.DB, fromRaw, toRaw string) (*gorm.DB, error) {
	if fromRaw != "" {
		from, err := parseDate(fromRaw, false)
		if err != nil {
			return query, err
		}
		query = query.Where("orders.paid_at >= ?", from)
	}
	if toRaw != "" {
		to, err := parseDate(toRaw, true)
		if err != nil {
			return query, err
		}
		query = query.Where("orders.paid_at < ?", to)
	}
	return query, nil
}

func parseDate(raw string, endExclusive bool) (time.Time, error) {
	if date, err := time.Parse("2006-01-02", raw); err == nil {
		if endExclusive {
			return date.AddDate(0, 0, 1), nil
		}
		return date, nil
	}
	value, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return time.Time{}, ValidationError{Message: "Некорректный период"}
	}
	if endExclusive {
		return value.Add(time.Nanosecond), nil
	}
	return value, nil
}

func (s Service) List(ctx context.Context, options ListOptions) (OrderPage, error) {
	if options.Page < 1 {
		options.Page = 1
	}
	query := s.db.WithContext(ctx).Model(&models.Order{})
	if options.Status != "" {
		if options.Status != models.OrderStatusPaid && options.Status != models.OrderStatusIssued {
			return OrderPage{}, ValidationError{Message: "Некорректный статус"}
		}
		query = query.Where("orders.status = ?", options.Status)
	}
	if q := strings.TrimSpace(options.Query); q != "" {
		like := "%" + strings.ToLower(q) + "%"
		query = query.Where("LOWER(orders.nick) LIKE ? OR LOWER(orders.order_id) LIKE ? OR LOWER(orders.yoo_kassa_id) LIKE ?", like, like, like)
	}
	var err error
	query, err = applyPeriod(query, options.From, options.To)
	if err != nil {
		return OrderPage{}, err
	}
	var total int64
	if err := query.Count(&total).Error; err != nil {
		return OrderPage{}, err
	}
	var items []models.Order
	if err := query.Preload("Deliveries", func(db *gorm.DB) *gorm.DB { return db.Order("id ASC") }).
		Order("orders.paid_at DESC, orders.id DESC").
		Offset((options.Page - 1) * PageSize).Limit(PageSize).Find(&items).Error; err != nil {
		return OrderPage{}, err
	}
	return OrderPage{Items: items, Total: total, Page: options.Page, PageSize: PageSize}, nil
}

func (s Service) Issue(ctx context.Context, orderID, adminID string) (models.Order, error) {
	if parsed, err := uuid.Parse(orderID); err != nil || parsed.String() != strings.ToLower(orderID) {
		return models.Order{}, ValidationError{Message: "Некорректный номер заказа"}
	}
	var order models.Order
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		now := time.Now().UTC()
		// Lock the same rows PollDeliveries claims. This makes "manual" and
		// "handed to game server" mutually exclusive instead of a COUNT/UPDATE race.
		var pending []models.Delivery
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("order_id = ? AND status = ?", orderID, models.DeliveryStatusPending).
			Order("id ASC").Find(&pending).Error; err != nil {
			return err
		}
		ids := make([]uint, 0, len(pending))
		for _, delivery := range pending {
			if delivery.ClaimedAt != nil && delivery.ClaimedAt.After(now.Add(-deliveryLease)) {
				return ErrDeliveryInFlight
			}
			ids = append(ids, delivery.ID)
		}
		if len(ids) > 0 {
			if err := tx.Model(&models.Delivery{}).Where("id IN ?", ids).
				Updates(map[string]any{"status": models.DeliveryStatusDone, "done_at": now}).Error; err != nil {
				return err
			}
		}
		result := tx.Model(&models.Order{}).
			Where("order_id = ? AND status <> ?", orderID, models.OrderStatusIssued).
			Updates(map[string]any{"status": models.OrderStatusIssued, "issued_at": now})
		if result.Error != nil {
			return result.Error
		}
		if err := tx.Preload("Deliveries", func(db *gorm.DB) *gorm.DB { return db.Order("id ASC") }).First(&order, "order_id = ?", orderID).Error; err != nil {
			return err
		}
		if result.RowsAffected == 1 && adminID != "" {
			details := fmt.Sprintf("order=%s nick=%s total=%d", order.OrderID, order.Nick, order.Total)
			if err := repo.InsertAudit(ctx, tx, nil, &adminID, nil, "admin_order_issue", &details); err != nil {
				return err
			}
		}
		return nil
	})
	return order, err
}

func (s Service) PollDeliveries(ctx context.Context, players []string) ([]models.Delivery, error) {
	if len(players) > 1000 {
		return nil, ValidationError{Message: "Слишком много игроков"}
	}
	names := make([]string, 0, len(players))
	seen := make(map[string]struct{}, len(players))
	for _, raw := range players {
		name := strings.ToLower(strings.TrimSpace(raw))
		if !nickPattern.MatchString(name) {
			continue
		}
		if _, ok := seen[name]; ok {
			continue
		}
		seen[name] = struct{}{}
		names = append(names, name)
	}
	if len(names) == 0 {
		return []models.Delivery{}, nil
	}
	var result []models.Delivery
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		now := time.Now().UTC()
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE", Options: "SKIP LOCKED"}).
			Where("status = ? AND LOWER(nick) IN ? AND (claimed_at IS NULL OR claimed_at <= ?)",
				models.DeliveryStatusPending, names, now.Add(-deliveryLease)).
			Order("id ASC").Limit(500).Find(&result).Error; err != nil {
			return err
		}
		if len(result) == 0 {
			return nil
		}
		ids := make([]uint, 0, len(result))
		for i := range result {
			ids = append(ids, result[i].ID)
			result[i].ClaimedAt = &now
		}
		return tx.Model(&models.Delivery{}).Where("id IN ?", ids).Update("claimed_at", now).Error
	})
	return result, err
}

func (s Service) AckDeliveries(ctx context.Context, ids []uint) error {
	if len(ids) > 1000 {
		return ValidationError{Message: "Слишком много подтверждений"}
	}
	if len(ids) == 0 {
		return nil
	}
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var orderIDs []string
		if err := tx.Model(&models.Delivery{}).Where("id IN ?", ids).Distinct("order_id").Pluck("order_id", &orderIDs).Error; err != nil {
			return err
		}
		now := time.Now().UTC()
		if err := tx.Model(&models.Delivery{}).Where("id IN ? AND status = ?", ids, models.DeliveryStatusPending).
			Updates(map[string]any{"status": models.DeliveryStatusDone, "done_at": now}).Error; err != nil {
			return err
		}
		for _, orderID := range orderIDs {
			var pending int64
			if err := tx.Model(&models.Delivery{}).Where("order_id = ? AND status = ?", orderID, models.DeliveryStatusPending).Count(&pending).Error; err != nil {
				return err
			}
			if pending == 0 {
				if err := tx.Model(&models.Order{}).Where("order_id = ? AND status <> ?", orderID, models.OrderStatusIssued).
					Updates(map[string]any{"status": models.OrderStatusIssued, "issued_at": now}).Error; err != nil {
					return err
				}
			}
		}
		return nil
	})
}

func (s Service) Stats(ctx context.Context, from, to string) (Stats, error) {
	base, err := applyPeriod(s.db.WithContext(ctx).Model(&models.Order{}), from, to)
	if err != nil {
		return Stats{}, err
	}
	var totals struct {
		Revenue int64
		Orders  int64
		Pending int64
	}
	if err := base.Select(
		"COALESCE(SUM(total), 0) AS revenue, COUNT(*) AS orders, "+
			"COALESCE(SUM(CASE WHEN status = ? THEN 1 ELSE 0 END), 0) AS pending",
		models.OrderStatusPaid,
	).Scan(&totals).Error; err != nil {
		return Stats{}, err
	}
	stats := Stats{
		Revenue: totals.Revenue,
		Orders:  totals.Orders,
		Pending: totals.Pending,
		Top:     make([]TopItem, 0),
	}

	topQuery := s.topItemsQuery(ctx)
	if topQuery == nil {
		return Stats{}, errors.New("unsupported database for order item statistics")
	}
	topQuery, err = applyPeriod(topQuery, from, to)
	if err != nil {
		return Stats{}, err
	}
	if err := topQuery.Group("1, 2").Order("qty DESC, revenue DESC, id ASC").Limit(10).Scan(&stats.Top).Error; err != nil {
		return Stats{}, err
	}
	return stats, nil
}

func (s Service) topItemsQuery(ctx context.Context) *gorm.DB {
	switch s.db.Dialector.Name() {
	case "postgres":
		return s.db.WithContext(ctx).
			Table("orders CROSS JOIN LATERAL jsonb_array_elements(orders.items::jsonb) AS item(value)").
			Select("CAST(item.value->>'id' AS BIGINT) AS id, item.value->>'name' AS name, " +
				"SUM(CAST(item.value->>'qty' AS INTEGER)) AS qty, " +
				"SUM(CAST(item.value->>'price' AS BIGINT) * CAST(item.value->>'qty' AS INTEGER)) AS revenue")
	case "sqlite":
		return s.db.WithContext(ctx).
			Table("orders JOIN json_each(orders.items) AS item").
			Select("CAST(json_extract(item.value, '$.id') AS INTEGER) AS id, " +
				"json_extract(item.value, '$.name') AS name, " +
				"SUM(CAST(json_extract(item.value, '$.qty') AS INTEGER)) AS qty, " +
				"SUM(CAST(json_extract(item.value, '$.price') AS INTEGER) * " +
				"CAST(json_extract(item.value, '$.qty') AS INTEGER)) AS revenue")
	default:
		return nil
	}
}
