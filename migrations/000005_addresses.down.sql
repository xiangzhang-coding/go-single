ALTER TABLE users DROP FOREIGN KEY fk_users_default_address;
ALTER TABLE users DROP COLUMN default_address_id;
DROP TABLE IF EXISTS user_addresses;
