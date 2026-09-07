package core

import (
	"sort"
	"strconv"

	"github.com/brandopakel/keel/internal/data_structure"
)

// Sorting has a separate 64 MiB workspace ceiling. Unordered replies walk
// without collecting points; COUNT keeps only its selected points in a heap.
const maxGeoPoints = MaxReplyBytes / 48

type geoWalk func(func(data_structure.GeoPoint) bool)

func geoSearchReply(z *data_structure.ZSet, radius data_structure.GeoHashRadius, s *geoSearch) []byte {
	var walk geoWalk = func(yield func(data_structure.GeoPoint) bool) {
		count := 0
		z.VisitGeoNeighbors(radius, &s.shape, func(p data_structure.GeoPoint) bool {
			count++
			return yield(p) && (!s.any || count < s.count)
		})
	}
	if s.order != "" {
		limit := s.count
		if limit <= 0 || limit > 1024 {
			// Large COUNT values do not establish how many points match. Count
			// first, stopping at the selection/workspace bound, so sparse shapes
			// neither reserve the entire set nor grow overlapping backing arrays.
			requested := limit
			limit = 0
			walk(func(data_structure.GeoPoint) bool {
				limit++
				return limit <= maxGeoPoints && (requested <= 0 || limit < requested)
			})
			if limit > maxGeoPoints {
				return replyTooLarge
			}
		}
		limit = min(limit, z.Len())
		if !reserveCommandMemory(limit*48 + 4096) {
			return allocationPressure
		}
		points := make([]data_structure.GeoPoint, 0, limit)
		better := func(a, b data_structure.GeoPoint) bool {
			if s.order == "DESC" {
				return a.Dist > b.Dist
			}
			return a.Dist < b.Dist
		}
		walk(func(p data_structure.GeoPoint) bool {
			if limit == 0 {
				return false
			}
			if len(points) < limit {
				points = append(points, p)
				// Worst selected point at the root, so replacement is O(log COUNT).
				for child := len(points) - 1; child > 0; {
					parent := (child - 1) / 2
					if !better(points[parent], points[child]) {
						break
					}
					points[parent], points[child] = points[child], points[parent]
					child = parent
				}
			} else if better(p, points[0]) {
				points[0] = p
				for parent := 0; 2*parent+1 < len(points); {
					child := 2*parent + 1
					if child+1 < len(points) && better(points[child], points[child+1]) {
						child++
					}
					if !better(points[parent], points[child]) {
						break
					}
					points[parent], points[child] = points[child], points[parent]
					parent = child
				}
			}
			return true
		})
		sort.Slice(points, func(i, j int) bool { return better(points[i], points[j]) })
		walk = func(yield func(data_structure.GeoPoint) bool) {
			for _, point := range points {
				if !yield(point) {
					break
				}
			}
		}
	}
	size, count, fits := 0, 0, true
	walk(func(p data_structure.GeoPoint) bool {
		count++
		var pointSize int
		pointSize, fits = geoPointReplySize(p, s)
		if !fits || pointSize > MaxReplyBytes-size {
			fits = false
			return false
		}
		size += pointSize
		return true
	})
	header := decimalDigits(count) + 3
	if !fits || size > MaxReplyBytes-header {
		return replyTooLarge
	}
	if !reserveReplyMemory(size + header) {
		return allocationPressure
	}
	out := appendArrayHeader(make([]byte, 0, size+header), count)
	walk(func(p data_structure.GeoPoint) bool { out = appendGeoPoint(out, p, s); return true })
	return out
}

func geoPointReplySize(p data_structure.GeoPoint, s *geoSearch) (int, bool) {
	size := 0
	if s.withDist || s.withHash || s.withCoord {
		size = 4
	}
	var fits bool
	size, fits = addBulkSize(size, len(p.Member))
	if !fits {
		return 0, false
	}
	var scratch [64]byte
	if s.withDist {
		size, fits = addBulkSize(size, len(strconv.AppendFloat(scratch[:0], p.Dist/s.shape.Conversion, 'f', 4, 64)))
		if !fits {
			return 0, false
		}
	}
	if s.withHash {
		n := len(strconv.AppendInt(scratch[:0], int64(p.Score), 10)) + 3
		if n > MaxReplyBytes-size {
			return 0, false
		}
		size += n
	}
	if s.withCoord {
		if size > MaxReplyBytes-4 {
			return 0, false
		}
		size += 4
		for _, v := range [2]float64{p.Longitude, p.Latitude} {
			size, fits = addBulkSize(size, len(strconv.AppendFloat(scratch[:0], v, 'f', -1, 64)))
			if !fits {
				return 0, false
			}
		}
	}
	return size, true
}

func appendGeoBulk(dst, body []byte) []byte {
	dst = append(dst, '$')
	dst = strconv.AppendInt(dst, int64(len(body)), 10)
	dst = append(dst, '\r', '\n')
	dst = append(dst, body...)
	return append(dst, '\r', '\n')
}

func appendGeoPoint(dst []byte, p data_structure.GeoPoint, s *geoSearch) []byte {
	fields := 1
	if s.withDist {
		fields++
	}
	if s.withHash {
		fields++
	}
	if s.withCoord {
		fields++
	}
	if fields > 1 {
		dst = appendArrayHeader(dst, fields)
	}
	dst = appendBulkString(dst, p.Member)
	var scratch [64]byte
	if s.withDist {
		dst = appendGeoBulk(dst, strconv.AppendFloat(scratch[:0], p.Dist/s.shape.Conversion, 'f', 4, 64))
	}
	if s.withHash {
		dst = append(dst, ':')
		dst = strconv.AppendInt(dst, int64(p.Score), 10)
		dst = append(dst, '\r', '\n')
	}
	if s.withCoord {
		dst = appendArrayHeader(dst, 2)
		dst = appendGeoBulk(dst, strconv.AppendFloat(scratch[:0], p.Longitude, 'f', -1, 64))
		dst = appendGeoBulk(dst, strconv.AppendFloat(scratch[:0], p.Latitude, 'f', -1, 64))
	}
	return dst
}
