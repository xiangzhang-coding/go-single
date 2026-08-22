import type { CreateAddressRequest, UpdateAddressRequest } from "../api/types";

export function toUpdateAddressRequest(request: CreateAddressRequest): UpdateAddressRequest {
  const { is_default: _isDefault, ...update } = request;
  return update;
}
