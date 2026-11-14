package deploy

import "sort"

func sortMaintenanceContainers(w *maintenance) {
	sort.Slice(w.Containers, func(i, k int) bool {
		if w.Containers[i].Name == w.Containers[k].Name {
			return w.Containers[i].ID < w.Containers[k].ID
		}
		return w.Containers[i].Name < w.Containers[k].Name
	})
	w.Plan.Containers = nil
	for _, c := range w.Containers {
		w.Plan.Containers = append(w.Plan.Containers, c.Name)
	}
}
