// Copyright 2017 orijtech. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package uber

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/orijtech/otils"
)

type ActivationStatus string

const (
	Other      ActivationStatus = "other"
	Onboarding ActivationStatus = "onboarding"
	Active     ActivationStatus = "active"
	InActive   ActivationStatus = "inactive"
)

const driverV1API = "v1"

func (c *Client) DriverProfile() (*Profile, error) {
	return c.retrieveProfile("/partners/me", driverV1API)
}

type PaymentCategory string

const (
	CategoryFare           PaymentCategory = "fare"
	CategoryDevicePayment  PaymentCategory = "device_payment"
	CategoryVehiclePayment PaymentCategory = "vehicle_payment"
	CategoryPromotion      PaymentCategory = "promotion"
	CategoryOther          PaymentCategory = "other"
)

const (
	defaultDriverPaymentsLimitPerPage = 50

	defaultThrottleDuration = 150 * time.Millisecond
)

type DriverInfoResponse struct {
	Cancel func()
	Pages  <-chan *DriverInfoPage
}

type driverInfoWrap struct {
	Count    int        `json:"count"`
	Limit    int        `json:"limit"`
	Offset   int        `json:"offset"`
	Payments []*Payment `json:"payments"`
	Trips    []*Trip    `json:"trips"`
}

func (dpq *DriverInfoQuery) toRealDriverQuery() *realDriverQuery {
	rdpq := &realDriverQuery{
		Offset: dpq.Offset,
	}
	if dpq.StartDate != nil {
		rdpq.StartTimeUnix = dpq.StartDate.Unix()
	}
	if dpq.EndDate != nil {
		rdpq.EndTimeUnix = dpq.EndDate.Unix()
	}
	return rdpq
}

// realDriverQuery because it is the 1-to-1 match
// with the fields sent it to query for the payments.
// DriverQuery is just there for convenience and
// easy API usage from callers e.g passing in a date without
// having to worry about its exact Unix timestamp.
type realDriverQuery struct {
	Offset        int   `json:"offset,omitempty"`
	LimitPerPage  int   `json:"limit,omitempty"`
	StartTimeUnix int64 `json:"from_time,omitempty"`
	EndTimeUnix   int64 `json:"to_time,omitempty"`
}

type DriverInfoQuery struct {
	Offset int `json:"offset,omitempty"`

	// LimitPerPage is the number of items to retrieve per page.
	// Default is 5, maximum is 50.
	LimitPerPage int `json:"limit,omitempty"`

	StartDate *time.Time `json:"start_date,omitempty"`
	EndDate   *time.Time `json:"end_date,omitempty"`

	MaxPageNumber int `json:"max_page_number,omitempty"`

	Throttle time.Duration `json:"throttle,omitempty"`
}

type DriverInfoPage struct {
	PageNumber int        `json:"page_number,omitempty"`
	Payments   []*Payment `json:"payments,omitempty"`
	Trips      []*Trip    `json:"trips,omitempty"`
	Err        error      `json:"error"`
}

func (c *Client) ListDriverTrips(dpq *DriverInfoQuery) (*DriverInfoResponse, error) {
	return c.listDriverInfo(dpq, "/partners/trips")
}

// DriverPayments returns the payments for the given driver.
// Payments are available at this endpoint in near real-time. Some entries,
// such as "device_subscription" will appear on a periodic basis when actually
// billed to the parnter. If a trip is cancelled (either by rider or driver) and
// there is no payment made, the corresponding "trip_id" of that cancelled trip
// will not appear in this endpoint. If the given driver works for a fleet manager,
// there will be no payments associated and the response will always be an empty
// array. Drivers working for fleet managers will receive payments from the fleet
// manager and not from Uber.
func (c *Client) ListDriverPayments(dpq *DriverInfoQuery) (*DriverInfoResponse, error) {
	return c.listDriverInfo(dpq, "/partners/payments")
}

func (c *Client) listDriverInfo(dpq *DriverInfoQuery, path string) (*DriverInfoResponse, error) {
	if dpq == nil {
		dpq = new(DriverInfoQuery)
	}

	throttleDuration := dpq.Throttle
	if throttleDuration == NoThrottle {
		throttleDuration = 0
	} else if throttleDuration <= 0 {
		throttleDuration = defaultThrottleDuration
	}

	maxPageNumber := dpq.MaxPageNumber
	pageExceeds := func(pageNumber int) bool {
		return maxPageNumber > 0 && pageNumber >= maxPageNumber
	}

	baseURL := fmt.Sprintf("%s%s", c.baseURL(driverV1API), path)
	rdpq := dpq.toRealDriverQuery()
	limitPerPage := rdpq.LimitPerPage
	if limitPerPage <= 0 {
		limitPerPage = defaultDriverPaymentsLimitPerPage
	}

	cancelChan, cancelFn := makeCancelParadigm()
	resChan := make(chan *DriverInfoPage)
	go func() {
		defer close(resChan)

		pageNumber := 0

		for {
			curPage := new(DriverInfoPage)
			curPage.PageNumber = pageNumber

			qv, err := otils.ToURLValues(rdpq)
			if err != nil {
				curPage.Err = err
				resChan <- curPage
				return
			}

			fullURL := baseURL
			if len(qv) > 0 {
				fullURL += "?" + qv.Encode()
			}

			req, err := http.NewRequest("GET", fullURL, nil)
			if err != nil {
				curPage.Err = err
				resChan <- curPage
				return
			}
			blob, _, err := c.doAuthAndHTTPReq(req)
			if err != nil {
				curPage.Err = err
				resChan <- curPage
				return
			}

			recv := new(driverInfoWrap)
			if err := json.Unmarshal(blob, recv); err != nil {
				curPage.Err = err
				resChan <- curPage
				return
			}

			// No payments nor trips sent back, so a sign that we are at the end
			if len(recv.Payments) == 0 && len(recv.Trips) == 0 {
				return
			}

			curPage.Trips = recv.Trips
			curPage.Payments = recv.Payments

			resChan <- curPage

			pageNumber += 1
			if pageExceeds(pageNumber) {
				return
			}

			select {
			case <-cancelChan:
				return
			case <-time.After(throttleDuration):
			}

			rdpq.Offset += recv.Limit
		}
	}()

	resp := &DriverInfoResponse{
		Cancel: cancelFn,
		Pages:  resChan,
	}

	return resp, nil
}
