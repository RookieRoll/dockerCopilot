export const SUCCESS_API_CODES = new Set([0, 200, 204])

export class ApiError extends Error {
  constructor(message, { status, errorCode, requestId, cause, response } = {}) {
    super(message || 'Request failed', cause === undefined ? undefined : { cause })
    this.name = 'ApiError'
    this.status = status
    this.errorCode = errorCode
    this.requestId = requestId
    this.response = response
  }
}

export function isSuccessApiCode(code) {
  return SUCCESS_API_CODES.has(code)
}

export function hasApiEnvelope(body) {
  return body !== null && typeof body === 'object' && !Array.isArray(body) && Object.hasOwn(body, 'code')
}

export function createBusinessApiError(response) {
  const body = response?.data
  return new ApiError(body?.msg || `Request failed with business code ${body?.code}`, {
    status: response?.status,
    errorCode: body?.errorCode ?? body?.code,
    requestId: body?.requestId ?? response?.headers?.['x-request-id'],
    response,
  })
}

export function normalizeHttpError(error) {
  if (error instanceof ApiError) return error
  const response = error?.response
  const body = response?.data
  return new ApiError(body?.msg || error?.message || 'Network request failed', {
    status: response?.status,
    errorCode: body?.errorCode ?? body?.code,
    requestId: body?.requestId ?? response?.headers?.['x-request-id'],
    cause: error?.cause ?? error,
    response,
  })
}
