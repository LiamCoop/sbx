-- Seed script for the branchable-database experiment.
-- Creates a small "Shop" database with customers and orders,
-- big enough (~100k customers, ~500k orders) to make ZFS space numbers meaningful.

IF DB_ID('Shop') IS NULL
    CREATE DATABASE Shop;
GO

USE Shop;
GO

DROP TABLE IF EXISTS dbo.Orders;
DROP TABLE IF EXISTS dbo.Customers;
GO

CREATE TABLE dbo.Customers (
    CustomerId INT IDENTITY PRIMARY KEY,
    Name       NVARCHAR(100) NOT NULL,
    Email      NVARCHAR(200) NOT NULL,
    CreatedAt  DATETIME2     NOT NULL DEFAULT SYSUTCDATETIME()
);

CREATE TABLE dbo.Orders (
    OrderId    INT IDENTITY PRIMARY KEY,
    CustomerId INT            NOT NULL REFERENCES dbo.Customers (CustomerId),
    Amount     DECIMAL(10, 2) NOT NULL,
    Status     VARCHAR(20)    NOT NULL,
    OrderedAt  DATETIME2      NOT NULL
);
GO

-- 100,000 customers
WITH n AS (
    SELECT TOP (100000) ROW_NUMBER() OVER (ORDER BY (SELECT NULL)) AS i
    FROM sys.all_objects a CROSS JOIN sys.all_objects b
)
INSERT dbo.Customers (Name, Email)
SELECT CONCAT('Customer ', i), CONCAT('customer', i, '@example.com')
FROM n;
GO

-- 500,000 orders spread across those customers over the past year
WITH n AS (
    SELECT TOP (500000) ROW_NUMBER() OVER (ORDER BY (SELECT NULL)) AS i
    FROM sys.all_objects a CROSS JOIN sys.all_objects b
)
INSERT dbo.Orders (CustomerId, Amount, Status, OrderedAt)
SELECT
    (i % 100000) + 1,
    CAST((ABS(CHECKSUM(NEWID())) % 50000) / 100.0 AS DECIMAL(10, 2)),
    CHOOSE((i % 4) + 1, 'pending', 'paid', 'shipped', 'refunded'),
    DATEADD(MINUTE, -(i % 525600), SYSUTCDATETIME())
FROM n;
GO

CREATE INDEX IX_Orders_CustomerId ON dbo.Orders (CustomerId);
GO

SELECT
    (SELECT COUNT(*) FROM dbo.Customers) AS Customers,
    (SELECT COUNT(*) FROM dbo.Orders)    AS Orders;
GO
