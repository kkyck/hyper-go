package main

// generic_helpers.go — Go generics for type-safe, zero-allocation helpers.

import (
	"github.com/polymorfa/hypermeow/types"
)

func contains[T comparable](slice []T, elem T) bool {
	for _, v := range slice {
		if v == elem {
			return true
		}
	}
	return false
}

func unique[T comparable](slice []T) []T {
	if len(slice) <= 1 {
		return slice
	}
	seen := make(map[T]struct{}, len(slice))
	result := make([]T, 0, len(slice))
	for _, v := range slice {
		if _, ok := seen[v]; ok {
			continue
		}
		seen[v] = struct{}{}
		result = append(result, v)
	}
	return result
}

func mapValues[T any, U any](slice []T, fn func(T) U) []U {
	result := make([]U, len(slice))
	for i, v := range slice {
		result[i] = fn(v)
	}
	return result
}

func filter[T any](slice []T, predicate func(T) bool) []T {
	result := make([]T, 0, len(slice))
	for _, v := range slice {
		if predicate(v) {
			result = append(result, v)
		}
	}
	return result
}

func firstOr[T any](slice []T, defaultVal T) T {
	if len(slice) == 0 {
		return defaultVal
	}
	return slice[0]
}

func dedupJIDs(jids []types.JID) []types.JID {
	if len(jids) <= 1 {
		return jids
	}
	seen := make(map[string]struct{}, len(jids))
	result := make([]types.JID, 0, len(jids))
	for _, j := range jids {
		key := j.User
		if key == "" {
			continue
		}
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		result = append(result, j)
	}
	return result
}
