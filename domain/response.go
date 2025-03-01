package domain

// MultiGetResponse represents the response from a GetMultiple request
type MultiGetResponse struct {
	LocalData       []byte           `json:"local_data"`
	RemoteLocations []RemoteLocation `json:"remote_locations"`
}

// RemoteLocation represents a location that is managed by a remote node
type RemoteLocation struct {
	Location string  `json:"location"`
	Contact  Contact `json:"contact"`
}
