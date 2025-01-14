/**
 * OpenBmclAPI (Golang Edition)
 * Copyright (C) 2024 Kevin Z <zyxkad@gmail.com>
 * All rights reserved
 *
 *  This program is free software: you can redistribute it and/or modify
 *  it under the terms of the GNU Affero General Public License as published
 *  by the Free Software Foundation, either version 3 of the License, or
 *  (at your option) any later version.
 *
 *  This program is distributed in the hope that it will be useful,
 *  but WITHOUT ANY WARRANTY; without even the implied warranty of
 *  MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
 *  GNU Affero General Public License for more details.
 *
 *  You should have received a copy of the GNU Affero General Public License
 *  along with this program.  If not, see <https://www.gnu.org/licenses/>.
 */

package utils

import (
	"fmt"
	"strconv"
	"strings"
)

func SplitCSV(line string) (values map[string]float32) {
	list := strings.Split(line, ",")
	values = make(map[string]float32, len(list))
	for _, v := range list {
		name, opt, _ := strings.Cut(strings.ToLower(strings.TrimSpace(v)), ";")
		var q float64 = 1
		if v, ok := strings.CutPrefix(opt, "q="); ok {
			q, _ = strconv.ParseFloat(v, 32)
		}
		values[name] = (float32)(q)
	}
	return
}

func BytesToUnit(size float64) string {
	if size < 1000 {
		return fmt.Sprintf("%dB", (int)(size))
	}
	size /= 1024
	unit := "KB"
	if size >= 1000 {
		size /= 1024
		unit = "MB"
		if size >= 1000 {
			size /= 1024
			unit = "GB"
			if size >= 1000 {
				size /= 1024
				unit = "TB"
			}
		}
	}
	return fmt.Sprintf("%.1f%s", size, unit)
}
