package main

type Product struct {
	ID                string      `gorm:"type:varchar(20);primaryKey" json:"id"`
	Name              string      `gorm:"type:varchar(255);not null" json:"name"`
	Description       string      `gorm:"type:text" json:"description"`
	Picture           string      `gorm:"type:varchar(255)" json:"picture"`
	PriceCurrencyCode string      `gorm:"type:varchar(10);not null" json:"-"`
	PriceUnits        int         `gorm:"not null" json:"-"`
	PriceNanos        int         `gorm:"not null" json:"-"`
	Categories        []*Category `gorm:"many2many:product_categories" json:"categories,omitempty"`
}

type Category struct {
	ID   int    `gorm:"primaryKey;autoIncrement" json:"-"`
	Name string `gorm:"type:varchar(50);unique;not null" json:"name"`
}

type ProductCategory struct {
	ProductID  string `gorm:"type:varchar(20);primaryKey"`
	CategoryID int    `gorm:"primaryKey"`
}
