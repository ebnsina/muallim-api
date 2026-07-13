-- +goose Up

/*
    The database still only believed in two gateways.

    00037 added the columns and the credentials table SSLCommerz and bKash need, and
    left the CHECK that names the gateways alone — so an account or an order for
    either one was refused by the database with a 23514, and the only way to find out
    was to try it. The drivers were written, wired, tested and unreachable.

    A constraint that enumerates values is a constraint that has to be revisited every
    time the set grows, and this is the revisit.
*/
ALTER TABLE payment_accounts DROP CONSTRAINT payment_accounts_gateway_check;
ALTER TABLE payment_accounts ADD CONSTRAINT payment_accounts_gateway_check
    CHECK (gateway IN ('stripe', 'sslcommerz', 'bkash', 'fake'));

ALTER TABLE orders DROP CONSTRAINT orders_gateway_check;
ALTER TABLE orders ADD CONSTRAINT orders_gateway_check
    CHECK (gateway IN ('stripe', 'sslcommerz', 'bkash', 'fake'));

-- +goose Down
ALTER TABLE payment_accounts DROP CONSTRAINT payment_accounts_gateway_check;
ALTER TABLE payment_accounts ADD CONSTRAINT payment_accounts_gateway_check
    CHECK (gateway IN ('stripe', 'fake'));

ALTER TABLE orders DROP CONSTRAINT orders_gateway_check;
ALTER TABLE orders ADD CONSTRAINT orders_gateway_check
    CHECK (gateway IN ('stripe', 'fake'));
