package cache

const acquireIdempotencyScript = `
if redis.call('SET', KEYS[1], ARGV[1], 'NX', 'EX', ARGV[2]) then
    return 1
end
return 0
`

const releaseIdempotencyScript = `
if redis.call('GET', KEYS[1]) == ARGV[1] then
    redis.call('DEL', KEYS[1])
    return 1
end
return 0
`

const productDetailVersionScript = `
return tonumber(redis.call('GET', KEYS[1]) or '0')
`

const setProductDetailIfVersionScript = `
local now = redis.call('TIME')
local now_ms = tonumber(now[1]) * 1000 + math.floor(tonumber(now[2]) / 1000)
redis.call('ZREMRANGEBYSCORE', KEYS[3], '-inf', now_ms)
if redis.call('ZCARD', KEYS[3]) > 0 then
    return 0
end
redis.call('DEL', KEYS[3])
local current = tonumber(redis.call('GET', KEYS[2]) or '0')
if current ~= tonumber(ARGV[1]) then
    return 0
end
redis.call('SET', KEYS[1], ARGV[2], 'PX', ARGV[3])
return 1
`

const beginProductDetailMutationScript = `
local now = redis.call('TIME')
local now_ms = tonumber(now[1]) * 1000 + math.floor(tonumber(now[2]) / 1000)
redis.call('ZREMRANGEBYSCORE', KEYS[3], '-inf', now_ms)
local expires_at = now_ms + tonumber(ARGV[2])
redis.call('ZADD', KEYS[3], expires_at, ARGV[1])
local latest = redis.call('ZRANGE', KEYS[3], -1, -1, 'WITHSCORES')
redis.call('PEXPIREAT', KEYS[3], latest[2])
redis.call('INCR', KEYS[2])
redis.call('DEL', KEYS[1])
return 1
`

const finishProductDetailMutationScript = `
redis.call('INCR', KEYS[2])
redis.call('DEL', KEYS[1])
local now = redis.call('TIME')
local now_ms = tonumber(now[1]) * 1000 + math.floor(tonumber(now[2]) / 1000)
redis.call('ZREM', KEYS[3], ARGV[1])
redis.call('ZREMRANGEBYSCORE', KEYS[3], '-inf', now_ms)
if redis.call('ZCARD', KEYS[3]) == 0 then
    redis.call('DEL', KEYS[3])
else
    local latest = redis.call('ZRANGE', KEYS[3], -1, -1, 'WITHSCORES')
    redis.call('PEXPIREAT', KEYS[3], latest[2])
end
return 1
`

const claimCouponScript = `
if ARGV[1] < ARGV[3] or ARGV[1] > ARGV[4] then
    return -1
end
local claimed_floor = tonumber(ARGV[6])
local version = tonumber(redis.call('GET', KEYS[3]) or '-1')
if claimed_floor > version then
    redis.call('SET', KEYS[3], claimed_floor)
end
local per_user_version = tonumber(redis.call('GET', KEYS[4]) or '-1')
local per_user_floor = tonumber(ARGV[7])
if per_user_floor > per_user_version then
    redis.call('SET', KEYS[4], per_user_floor)
end
local claimed = tonumber(redis.call('GET', KEYS[1]) or '0')
if claimed < claimed_floor then
    claimed = claimed_floor
    redis.call('SET', KEYS[1], claimed)
end
if claimed >= tonumber(ARGV[2]) then
    return 0
end
local per_user = tonumber(redis.call('GET', KEYS[2]) or '0')
if per_user < per_user_floor then
    per_user = per_user_floor
    redis.call('SET', KEYS[2], per_user)
end
if per_user >= tonumber(ARGV[5]) then
    return -2
end
redis.call('INCR', KEYS[1])
redis.call('INCR', KEYS[2])
return 1
`

const syncCouponCountsScript = `
local updated = 0
local version = tonumber(redis.call('GET', KEYS[3]) or '-1')
if tonumber(ARGV[1]) >= version then
    redis.call('SET', KEYS[1], ARGV[1])
    redis.call('SET', KEYS[3], ARGV[1])
    updated = 1
end
local per_user_version = tonumber(redis.call('GET', KEYS[4]) or '-1')
if tonumber(ARGV[2]) >= per_user_version then
    redis.call('SET', KEYS[2], ARGV[2])
    redis.call('SET', KEYS[4], ARGV[2])
    updated = 1
end
return updated
`

const warmFlashSaleStockScript = `
local cur = tonumber(redis.call('GET', KEYS[1]) or '-1')
if ARGV[3] == '1' or cur < 0 or tonumber(ARGV[1]) < cur then
    redis.call('SET', KEYS[1], ARGV[1], 'EX', ARGV[2])
    return 1
end
local ttl = redis.call('PTTL', KEYS[1])
if ttl > 0 then
    redis.call('SET', KEYS[1], cur, 'PX', ttl)
else
    redis.call('SET', KEYS[1], cur)
end
return 0
`

const decreaseFlashSaleStockScript = `
local current = tonumber(redis.call('GET', KEYS[1]) or '-1')
local delta = tonumber(ARGV[1])
if current < 0 then
    return redis.error_reply('flash-sale stock is missing during decrease')
end
if delta == nil or delta <= 0 then
    return redis.error_reply('flash-sale stock decrease must be positive')
end
if current < delta then
    return redis.error_reply('flash-sale stock decrease exceeds sellable stock')
end
redis.call('DECRBY', KEYS[1], delta)
return 1
`

const pauseFlashSaleStockScript = `
local stock = tonumber(redis.call('GET', KEYS[1]) or '-1')
if stock < 0 then
    return redis.error_reply('flash-sale stock is missing during pause')
end
if not redis.call('SET', KEYS[2], ARGV[2], 'NX', 'EX', ARGV[1]) then
    return redis.error_reply('flash-sale stock is already paused')
end
return stock
`

const releaseFlashSalePauseScript = `
if ARGV[1] == '' or redis.call('GET', KEYS[1]) == ARGV[1] then
    redis.call('DEL', KEYS[1])
end
return 1
`

const holdFlashSalePauseScript = `
redis.call('SET', KEYS[1], 'fail-closed')
return 1
`

const preDeductFlashSaleScript = `
if KEYS[3] ~= nil and KEYS[3] ~= '' then
    local reservation = redis.call('GET', KEYS[3])
    if reservation == ARGV[6] then
        local stock_value = redis.call('GET', KEYS[1])
        local count_value = redis.call('GET', KEYS[2])
        if stock_value == false or count_value == false then
            return redis.error_reply('flash-sale reserved state is incomplete')
        end
        local stock_ttl = redis.call('PTTL', KEYS[1])
        if stock_ttl > 0 then
            redis.call('SET', KEYS[1], stock_value, 'PX', stock_ttl)
        else
            redis.call('SET', KEYS[1], stock_value)
        end
        redis.call('SET', KEYS[2], count_value)
        redis.call('SET', KEYS[3], ARGV[6])
        if KEYS[4] ~= nil and KEYS[4] ~= '' then
            redis.call('SET', KEYS[4], ARGV[6], 'EX', ARGV[8])
        end
        return 2
    end
    if reservation ~= false then
        return redis.error_reply('flash-sale reservation token mismatch')
    end
end
if KEYS[5] ~= nil and KEYS[5] ~= '' and redis.call('EXISTS', KEYS[5]) == 1 then
    return -4
end
if ARGV[4] ~= '1' then
    return -3
end
if ARGV[1] < ARGV[2] or ARGV[1] > ARGV[3] then
    return -1
end
local stock = tonumber(redis.call('GET', KEYS[1]) or '0')
if stock <= 0 then
    return 0
end
local per_user = tonumber(redis.call('GET', KEYS[2]) or '0')
if per_user >= tonumber(ARGV[5]) then
    return -2
end
redis.call('DECR', KEYS[1])
redis.call('INCR', KEYS[2])
if KEYS[3] ~= nil and KEYS[3] ~= '' then
    if tonumber(ARGV[7]) > 0 then
        redis.call('SET', KEYS[3], ARGV[6], 'EX', ARGV[7])
    else
        redis.call('SET', KEYS[3], ARGV[6])
    end
end
if KEYS[4] ~= nil and KEYS[4] ~= '' then
    redis.call('SET', KEYS[4], ARGV[6], 'EX', ARGV[8])
end
return 1
`

const restoreFlashSaleScript = `
local function rewrite_string(key)
    if redis.call('EXISTS', key) == 0 then
        return
    end
    local value = redis.call('GET', key)
    local ttl = redis.call('PTTL', key)
    if ttl > 0 then
        redis.call('SET', key, value, 'PX', ttl)
    else
        redis.call('SET', key, value)
    end
end
local function parse_safe_integer(value)
    if value ~= '0' and value ~= '-0' and
       string.match(value, '^[1-9]%d*$') == nil and
       string.match(value, '^%-[1-9]%d*$') == nil then
        return nil
    end
    local number = tonumber(value)
    if number == nil or number > 9007199254740991 or number < -9007199254740991 then
        return nil
    end
    return number
end
local quantity = parse_safe_integer(ARGV[1])
if quantity == nil or quantity <= 0 then
    return redis.error_reply('flash-sale restore quantity is not a positive safe integer')
end
local scoped = KEYS[4] ~= nil and KEYS[4] ~= ''
local count_delta = quantity
if scoped then
    count_delta = 1
end
local should_restore = not scoped
if scoped then
    local reservation = redis.call('GET', KEYS[4])
    if reservation == 'rolled_back:' .. ARGV[2] then
        rewrite_string(KEYS[1])
        rewrite_string(KEYS[2])
        redis.call('SET', KEYS[4], reservation)
        return 2
    end
    if reservation ~= false and reservation ~= ARGV[2] then
        return redis.error_reply('flash-sale reservation token mismatch')
    end
    should_restore = reservation == ARGV[2]
    if not should_restore and ARGV[3] == '1' then
        should_restore = redis.call('GET', KEYS[3]) == ARGV[2]
    end
    if not should_restore then
        if redis.call('GET', KEYS[3]) == ARGV[2] then
            redis.call('DEL', KEYS[3])
        end
        if reservation == false and ARGV[4] == '1' then
            if redis.call('EXISTS', KEYS[1]) == 0 then
                redis.call('SET', KEYS[1], ARGV[5], 'EX', ARGV[6])
            end
            redis.call('SET', KEYS[4], 'rolled_back:' .. ARGV[2])
            return 1
        end
        return 0
    end
end
local stock_exists = redis.call('EXISTS', KEYS[1]) == 1
local count_exists = redis.call('EXISTS', KEYS[2]) == 1
if stock_exists and parse_safe_integer(redis.call('GET', KEYS[1])) == nil then
    return redis.error_reply('flash-sale stock is not a safe integer')
end
if count_exists and parse_safe_integer(redis.call('GET', KEYS[2])) == nil then
    return redis.error_reply('flash-sale count is not a safe integer')
end
if stock_exists then
    redis.call('INCRBY', KEYS[1], ARGV[1])
end
if count_exists then
    redis.call('DECRBY', KEYS[2], count_delta)
end
if redis.call('GET', KEYS[3]) == ARGV[2] or not scoped then
    redis.call('DEL', KEYS[3])
end
if scoped then
    redis.call('SET', KEYS[4], 'rolled_back:' .. ARGV[2])
end
return 1
`

const ensureFlashSaleReservationScript = `
local reservation = redis.call('GET', KEYS[4])
if reservation ~= false and reservation ~= ARGV[1] then
    return redis.error_reply('flash-sale reservation token mismatch')
end
local idem = redis.call('GET', KEYS[3])
if idem ~= false and idem ~= ARGV[1] then
    return redis.error_reply('flash-sale idempotency token mismatch')
end
local quantity = tonumber(ARGV[2])
local stock_raw = redis.call('GET', KEYS[1])
local stock
if stock_raw == false then
    stock = tonumber(ARGV[4])
    if stock == nil then
        return redis.error_reply('flash-sale fallback stock is invalid during reservation recovery')
    end
    redis.call('SET', KEYS[1], stock, 'EX', ARGV[5])
else
    stock = tonumber(stock_raw)
    if stock == nil then
        return redis.error_reply('flash-sale stock is invalid during reservation recovery')
    end
end
local count = tonumber(redis.call('GET', KEYS[2]) or '0')
if count == nil then
    return redis.error_reply('flash-sale count is invalid during reservation recovery')
end
if reservation == ARGV[1] then
    redis.call('SET', KEYS[1], stock, 'EX', ARGV[5])
    redis.call('SET', KEYS[2], count)
    redis.call('SET', KEYS[3], ARGV[1], 'EX', ARGV[3])
    redis.call('SET', KEYS[4], ARGV[1])
    return 1
end
if stock < quantity then
    return redis.error_reply('flash-sale stock is insufficient during reservation recovery')
end
redis.call('DECRBY', KEYS[1], quantity)
redis.call('INCR', KEYS[2])
redis.call('SET', KEYS[4], ARGV[1])
if idem == false then
    redis.call('SET', KEYS[3], ARGV[1], 'EX', ARGV[3])
end
return 2
`

const ensureOrderedFlashSaleReservationScript = `
local reservation = redis.call('GET', KEYS[4])
if reservation ~= false and reservation ~= ARGV[1] then
    return redis.error_reply('flash-sale ordered reservation token mismatch')
end
local idem = redis.call('GET', KEYS[3])
if idem ~= false and idem ~= ARGV[1] then
    return redis.error_reply('flash-sale ordered idempotency token mismatch')
end
local stock_raw = redis.call('GET', KEYS[1])
local stock = tonumber(stock_raw or '-1')
local fallback_stock = tonumber(ARGV[4])
if fallback_stock == nil or stock == nil then
    return redis.error_reply('flash-sale ordered stock is invalid during reservation recovery')
end
if stock_raw == false or fallback_stock < stock then
    redis.call('SET', KEYS[1], fallback_stock, 'EX', ARGV[5])
    stock = fallback_stock
end
local count = tonumber(redis.call('GET', KEYS[2]) or '0')
if count == nil then
    return redis.error_reply('flash-sale ordered count is invalid during reservation recovery')
end
if reservation == ARGV[1] then
    redis.call('SET', KEYS[1], stock, 'EX', ARGV[5])
    redis.call('SET', KEYS[2], count)
    redis.call('SET', KEYS[3], ARGV[1], 'EX', ARGV[3])
    redis.call('SET', KEYS[4], ARGV[1])
    return 1
end
redis.call('INCR', KEYS[2])
redis.call('SET', KEYS[4], ARGV[1])
if idem == false then
    redis.call('SET', KEYS[3], ARGV[1], 'EX', ARGV[3])
end
return 2
`

const adoptLegacyFlashSaleReservationScript = `
local reservation = redis.call('GET', KEYS[4])
if reservation ~= false and reservation ~= ARGV[1] then
    return redis.error_reply('legacy flash-sale reservation token mismatch')
end
local idem = redis.call('GET', KEYS[3])
if idem ~= false and idem ~= ARGV[1] then
    return redis.error_reply('legacy flash-sale idempotency token mismatch')
end
local stock = redis.call('GET', KEYS[1])
local target_stock = tonumber(ARGV[3])
if target_stock == nil then
    return redis.error_reply('legacy flash-sale target stock is invalid')
end
if stock == false or tonumber(stock) == nil or target_stock < tonumber(stock) then
    stock = target_stock
end
if redis.call('EXISTS', KEYS[1]) == 1 then
    local stock_ttl = redis.call('PTTL', KEYS[1])
    if stock_ttl > 0 then
        redis.call('SET', KEYS[1], stock, 'PX', stock_ttl)
    else
        redis.call('SET', KEYS[1], stock)
    end
else
    redis.call('SET', KEYS[1], stock, 'EX', ARGV[5])
end
local count = redis.call('GET', KEYS[2])
local target_count = tonumber(ARGV[4])
if target_count == nil then
    return redis.error_reply('legacy flash-sale target count is invalid')
end
if count == false or tonumber(count) == nil or tonumber(count) < target_count then
    count = target_count
end
if tonumber(count) == nil then
    return redis.error_reply('legacy flash-sale count is invalid')
end
redis.call('SET', KEYS[2], count)
redis.call('SET', KEYS[3], ARGV[1], 'EX', ARGV[2])
redis.call('SET', KEYS[4], ARGV[1])
if reservation == ARGV[1] then
    return 1
end
return 2
`

const incrementFixedWindowScript = `
if redis.call('EXISTS', KEYS[1]) == 0 then
    redis.call('SET', KEYS[1], 1, 'EX', ARGV[1])
    return 1
end
return redis.call('INCR', KEYS[1])
`
