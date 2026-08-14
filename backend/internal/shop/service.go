package shop

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"launcher-backend/internal/models"

	"gorm.io/gorm"
)

const maxImageBytes = 10 * 1024 * 1024

var ErrValidation = errors.New("shop validation")
var itemIDPattern = regexp.MustCompile(`^[a-z0-9_.-]+:[a-z0-9_./-]+$`)
var paidRoles = map[string]struct{}{"special_forces": {}, "uav_operator": {}, "ew_specialist": {}}

type CatalogItem struct {
	ID           int64               `json:"id"`
	Category     string              `json:"category"`
	CategoryIcon string              `json:"categoryIcon"`
	Name         string              `json:"name"`
	Description  string              `json:"description"`
	Price        int64               `json:"price"`
	ImageURL     string              `json:"imageUrl,omitempty"`
	Badge        string              `json:"badge,omitempty"`
	Sort         int                 `json:"sort"`
	Active       bool                `json:"active"`
	Delivery     models.DeliverySpec `json:"delivery"`
}

type ItemInput struct {
	ID           int64               `json:"id"`
	Category     string              `json:"category"`
	CategoryIcon string              `json:"categoryIcon"`
	Name         string              `json:"name"`
	Description  string              `json:"description"`
	Price        int64               `json:"price"`
	Badge        string              `json:"badge"`
	Sort         int                 `json:"sort"`
	Active       bool                `json:"active"`
	Delivery     models.DeliverySpec `json:"delivery"`
}

type Service struct {
	db          *gorm.DB
	storageRoot string
}

func NewService(db *gorm.DB, storageRoot string) Service {
	return Service{db: db, storageRoot: storageRoot}
}

func validation(message string) error { return fmt.Errorf("%w: %s", ErrValidation, message) }

func cleanDelivery(spec models.DeliverySpec) (models.DeliverySpec, error) {
	spec.Type = strings.ToLower(strings.TrimSpace(spec.Type))
	spec.RoleID = strings.ToLower(strings.TrimSpace(spec.RoleID))
	spec.ItemID = strings.ToLower(strings.TrimSpace(spec.ItemID))
	switch spec.Type {
	case models.DeliveryTypeNone:
		return models.DeliverySpec{Type: models.DeliveryTypeNone}, nil
	case models.DeliveryTypeRole:
		if _, ok := paidRoles[spec.RoleID]; !ok {
			return spec, validation("Некорректный id роли")
		}
		return models.DeliverySpec{Type: spec.Type, RoleID: spec.RoleID}, nil
	case models.DeliveryTypeItem:
		if !itemIDPattern.MatchString(spec.ItemID) {
			return spec, validation("Некорректный id предмета")
		}
		if spec.Count < 1 || spec.Count > 100 {
			return spec, validation("Количество предметов должно быть от 1 до 100")
		}
		return models.DeliverySpec{Type: spec.Type, ItemID: spec.ItemID, Count: spec.Count}, nil
	default:
		return spec, validation("Некорректный тип выдачи")
	}
}

func cleanInput(input ItemInput) (ItemInput, error) {
	input.Category = strings.TrimSpace(input.Category)
	input.CategoryIcon = strings.TrimSpace(input.CategoryIcon)
	input.Name = strings.TrimSpace(input.Name)
	input.Description = strings.TrimSpace(input.Description)
	input.Badge = strings.TrimSpace(input.Badge)
	if input.ID <= 0 {
		return input, validation("Некорректный id товара")
	}
	if input.Category == "" || len(input.Category) > 100 {
		return input, validation("Некорректная категория")
	}
	if input.CategoryIcon == "" || len(input.CategoryIcon) > 64 {
		return input, validation("Некорректная иконка категории")
	}
	if input.Name == "" || len(input.Name) > 300 {
		return input, validation("Некорректное название")
	}
	if len(input.Description) > 5000 {
		return input, validation("Слишком длинное описание")
	}
	if input.Price < 1 || input.Price > 100_000_000 {
		return input, validation("Некорректная цена")
	}
	if len(input.Badge) > 80 {
		return input, validation("Слишком длинный бейдж")
	}
	var err error
	input.Delivery, err = cleanDelivery(input.Delivery)
	return input, err
}

func modelFromInput(input ItemInput) models.ShopItem {
	return models.ShopItem{ID: input.ID, Category: input.Category, CategoryIcon: input.CategoryIcon,
		Name: input.Name, Description: input.Description, Price: input.Price, Badge: input.Badge,
		Sort: input.Sort, Active: input.Active, Delivery: input.Delivery}
}

func catalogItem(item models.ShopItem) CatalogItem {
	result := CatalogItem{ID: item.ID, Category: item.Category, CategoryIcon: item.CategoryIcon,
		Name: item.Name, Description: item.Description, Price: item.Price, Badge: item.Badge,
		Sort: item.Sort, Active: item.Active, Delivery: item.Delivery}
	if item.ImagePath != "" {
		result.ImageURL = fmt.Sprintf("/api/shop/images/%d.png?v=%d", item.ID, item.UpdatedAt.Unix())
	}
	return result
}

func (s Service) List(ctx context.Context, activeOnly bool) ([]CatalogItem, error) {
	query := s.db.WithContext(ctx).Model(&models.ShopItem{})
	if activeOnly {
		query = query.Where("active = ?", true)
	}
	var rows []models.ShopItem
	if err := query.Order("sort ASC, id ASC").Find(&rows).Error; err != nil {
		return nil, err
	}
	items := make([]CatalogItem, 0, len(rows))
	for _, row := range rows {
		items = append(items, catalogItem(row))
	}
	return items, nil
}

func (s Service) Create(ctx context.Context, input ItemInput) (CatalogItem, error) {
	clean, err := cleanInput(input)
	if err != nil {
		return CatalogItem{}, err
	}
	item := modelFromInput(clean)
	if err := s.db.WithContext(ctx).Create(&item).Error; err != nil {
		return CatalogItem{}, err
	}
	return catalogItem(item), nil
}

func (s Service) Update(ctx context.Context, id int64, input ItemInput) (CatalogItem, error) {
	input.ID = id
	clean, err := cleanInput(input)
	if err != nil {
		return CatalogItem{}, err
	}
	var existing models.ShopItem
	if err := s.db.WithContext(ctx).First(&existing, "id = ?", id).Error; err != nil {
		return CatalogItem{}, err
	}
	image := existing.ImagePath
	item := modelFromInput(clean)
	item.ImagePath = image
	if err := s.db.WithContext(ctx).Model(&existing).Select("category", "category_icon", "name", "description", "price", "badge", "sort", "active", "delivery").Updates(&item).Error; err != nil {
		return CatalogItem{}, err
	}
	if err := s.db.WithContext(ctx).First(&existing, "id = ?", id).Error; err != nil {
		return CatalogItem{}, err
	}
	return catalogItem(existing), nil
}

func (s Service) Delete(ctx context.Context, id int64) error {
	var item models.ShopItem
	if err := s.db.WithContext(ctx).First(&item, "id = ?", id).Error; err != nil {
		return err
	}
	if err := s.db.WithContext(ctx).Delete(&item).Error; err != nil {
		return err
	}
	if item.ImagePath != "" {
		_ = os.Remove(filepath.Join(s.storageRoot, item.ImagePath))
	}
	return nil
}

func (s Service) SaveImage(ctx context.Context, id int64, reader io.Reader) (CatalogItem, error) {
	var item models.ShopItem
	if err := s.db.WithContext(ctx).First(&item, "id = ?", id).Error; err != nil {
		return CatalogItem{}, err
	}
	data, err := io.ReadAll(io.LimitReader(reader, maxImageBytes+1))
	if err != nil {
		return CatalogItem{}, err
	}
	if len(data) == 0 || len(data) > maxImageBytes || http.DetectContentType(data) != "image/png" || !bytes.HasPrefix(data, []byte("\x89PNG\r\n\x1a\n")) {
		return CatalogItem{}, validation("Нужен PNG до 10 МБ")
	}
	if err := os.MkdirAll(s.storageRoot, 0755); err != nil {
		return CatalogItem{}, err
	}
	tmp, err := os.CreateTemp(s.storageRoot, ".shop-image-*.tmp")
	if err != nil {
		return CatalogItem{}, err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return CatalogItem{}, err
	}
	if err := tmp.Close(); err != nil {
		return CatalogItem{}, err
	}
	name := fmt.Sprintf("%d.png", id)
	if err := os.Rename(tmpName, filepath.Join(s.storageRoot, name)); err != nil {
		return CatalogItem{}, err
	}
	if err := s.db.WithContext(ctx).Model(&item).Update("image_path", name).Error; err != nil {
		return CatalogItem{}, err
	}
	item.ImagePath = name
	return catalogItem(item), nil
}

func (s Service) ImagePath(ctx context.Context, id int64) (string, error) {
	var item models.ShopItem
	if err := s.db.WithContext(ctx).Select("id", "image_path").First(&item, "id = ?", id).Error; err != nil {
		return "", err
	}
	if item.ImagePath == "" {
		return "", gorm.ErrRecordNotFound
	}
	return filepath.Join(s.storageRoot, filepath.Base(item.ImagePath)), nil
}
