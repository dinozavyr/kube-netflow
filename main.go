package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"image/color"
	"log"
	"math"
	"net"
	"strings"
	"time"

	"github.com/elastic/go-elasticsearch/v8"
	"gonum.org/v1/plot"
	"gonum.org/v1/plot/vg"
	"gonum.org/v1/plot/vg/draw"
)

func cidrToRange(cidr string) (string, string) {
	_, ipNet, err := net.ParseCIDR(cidr)
	if err != nil {
		log.Fatalf("Invalid CIDR notation: %s", cidr)
	}

	network := ipNet.IP
	broadcast := make(net.IP, len(network))
	copy(broadcast, network)
	for i := range broadcast {
		broadcast[i] |= ^ipNet.Mask[i]
	}

	return network.String(), broadcast.String()
}

type NetworkFlow struct {
	Source      string    `json:"source.ip"`
	Destination string    `json:"destination.ip"`
	Bytes       int64     `json:"network.bytes"`
	Timestamp   time.Time `json:"@timestamp"`
}

type ChordDiagram struct {
	Flow   [][]float64
	Labels []string
	Color  func(i, j int) color.Color
}

func (c ChordDiagram) Plot(canvas draw.Canvas, plt *plot.Plot) {
	origin := vg.Point{X: canvas.Size().X / 2, Y: canvas.Size().Y / 2}
	radius := math.Min(float64(canvas.Size().X), float64(canvas.Size().Y)) * 0.35

	n := len(c.Flow)
	angleStep := 2 * math.Pi / float64(n)

	outerLabelFont := plot.DefaultFont
	outerLabelFont.Size = vg.Length(12)
	outerLabelStyle := draw.TextStyle{
		Color:   color.Black,
		Font:    outerLabelFont,
		Handler: plot.DefaultTextHandler,
	}

	baseLabelFont := plot.DefaultFont
	baseLabelFont.Size = vg.Length(12)
	baseLabelStyle := draw.TextStyle{
		Color:   color.Black,
		Font:    baseLabelFont,
		Handler: plot.DefaultTextHandler,
	}

	for i := 0; i < n; i++ {
		angle := float64(i) * angleStep
		startAngle := angle - angleStep/3
		endAngle := angle + angleStep/3

		var path vg.Path
		path.Move(pointOnCircle(origin, vg.Length(radius), startAngle))
		path.Arc(origin, vg.Length(radius), startAngle, endAngle-startAngle)
		canvas.SetLineWidth(vg.Points(2)) // Thicker arc lines
		canvas.SetColor(color.RGBA{100, 100, 100, 255})
		canvas.Stroke(path)

		if c.Labels != nil {
			baseAngle := angle
			basePos := pointOnCircle(origin, vg.Length(radius*1.07), baseAngle)

			baseRotation := baseAngle
			if baseAngle > math.Pi/2 && baseAngle < 3*math.Pi/2 {
				baseRotation += math.Pi
			}
			baseLabelStyle.Rotation = baseRotation
			baseLabelStyle.XAlign = draw.XCenter
			baseLabelStyle.YAlign = draw.YCenter

			bgBaseStyle := baseLabelStyle
			bgBaseStyle.Color = color.RGBA{255, 255, 255, 220}
			canvas.FillText(bgBaseStyle, basePos, c.Labels[i])
			canvas.FillText(baseLabelStyle, basePos, c.Labels[i])

			labelAngle := angle
			labelRotation := labelAngle + math.Pi/2
			if labelAngle > math.Pi/2 && labelAngle < 3*math.Pi/2 {
				labelRotation += math.Pi
			}

			totalBytes := float64(0)
			for j := 0; j < n; j++ {
				totalBytes += c.Flow[i][j]
			}
			statsLabel := fmt.Sprintf("%.1f MB", totalBytes/1024/1024) // Convert to MB

			labelPos := pointOnCircle(origin, vg.Length(radius*1.15), angle)
			outerLabelStyle.Rotation = labelRotation
			outerLabelStyle.XAlign = draw.XCenter
			outerLabelStyle.YAlign = draw.YCenter

			bgStyle := outerLabelStyle
			bgStyle.Color = color.RGBA{255, 255, 255, 220}
			canvas.FillText(bgStyle, labelPos, statsLabel)
			canvas.FillText(outerLabelStyle, labelPos, statsLabel)
		}
	}

	maxFlow := 0.0
	for i := range c.Flow {
		for j := range c.Flow[i] {
			if c.Flow[i][j] > maxFlow {
				maxFlow = c.Flow[i][j]
			}
		}
	}

	for i := range c.Flow {
		for j := range c.Flow[i] {
			if c.Flow[i][j] > 0 {
				weight := c.Flow[i][j] / maxFlow
				drawChord(canvas, origin, vg.Length(radius), i, j, n, weight, c.Color(i, j))
			}
		}
	}
}

func pointOnCircle(origin vg.Point, radius vg.Length, angle float64) vg.Point {
	return vg.Point{
		X: origin.X + radius*vg.Length(math.Cos(angle)),
		Y: origin.Y + radius*vg.Length(math.Sin(angle)),
	}
}

func drawChord(canvas draw.Canvas, origin vg.Point, radius vg.Length, i, j, n int, weight float64, clr color.Color) {
	angleStep := 2 * math.Pi / float64(n)
	angle1 := float64(i) * angleStep
	angle2 := float64(j) * angleStep

	start := pointOnCircle(origin, radius, angle1)
	end := pointOnCircle(origin, radius, angle2)

	var path vg.Path
	path.Move(start)

	ctrl1 := vg.Point{
		X: origin.X + radius*0.5*vg.Length(math.Cos(angle1)),
		Y: origin.Y + radius*0.5*vg.Length(math.Sin(angle1)),
	}
	ctrl2 := vg.Point{
		X: origin.X + radius*0.5*vg.Length(math.Cos(angle2)),
		Y: origin.Y + radius*0.5*vg.Length(math.Sin(angle2)),
	}

	path.CubeTo(ctrl1, ctrl2, end)

	canvas.SetLineWidth(vg.Length(weight * 3))
	rgba := color.RGBAModel.Convert(clr).(color.RGBA)
	rgba.A = uint8(math.Min(255, float64(rgba.A)+100))
	canvas.SetColor(rgba)
	canvas.Stroke(path)
}

func main() {

	timeWindowPtr := flag.String("window", "3h", "Time window for data (e.g., 15m, 1h, 24h)")
	networkFilterPtr := flag.String("network", "10.0.0.0/8", "Network CIDR filter (e.g., '10.0.0.0/8,192.168.0.0/16')")
	flag.Parse()

	var networkFilters []string
	if *networkFilterPtr != "" {
		networkFilters = strings.Split(*networkFilterPtr, ",")

		cfg := elasticsearch.Config{
			Addresses: []string{"https://es.dinozavyr.com:443"},
			Username:  "elastic",
			Password:  "Ra4Zb9R52151X2bzq9dlQI7v",
		}
		es, err := elasticsearch.NewClient(cfg)
		if err != nil {
			log.Fatalf("Error creating client: %s", err)
		}

		var conditions []map[string]interface{}
		if len(networkFilters) > 0 {
			var networkConditions []map[string]interface{}
			for _, cidr := range networkFilters {
				networkStart, networkEnd := cidrToRange(cidr)
				networkConditions = append(networkConditions,
					map[string]interface{}{
						"bool": map[string]interface{}{
							"must": []map[string]interface{}{
								{
									"range": map[string]interface{}{
										"source.ip": map[string]interface{}{
											"gte": networkStart,
											"lte": networkEnd,
										},
									},
								},
								{
									"range": map[string]interface{}{
										"destination.ip": map[string]interface{}{
											"gte": networkStart,
											"lte": networkEnd,
										},
									},
								},
							},
						},
					},
				)
			}
			conditions = append(conditions, map[string]interface{}{
				"bool": map[string]interface{}{
					"must": networkConditions,
				},
			})
		}

		conditions = append(conditions, map[string]interface{}{
			"range": map[string]interface{}{
				"@timestamp": map[string]interface{}{
					"gte": fmt.Sprintf("now-%s", *timeWindowPtr),
					"lte": "now",
				},
			},
		})

		query := map[string]interface{}{
			"size": 0,
			"query": map[string]interface{}{
				"bool": map[string]interface{}{
					"must": conditions,
				},
			},
			"aggs": map[string]interface{}{
				"source_nodes": map[string]interface{}{
					"terms": map[string]interface{}{
						"field": "source.ip",
						"size":  100,
					},
					"aggs": map[string]interface{}{
						"destinations": map[string]interface{}{
							"terms": map[string]interface{}{
								"field": "destination.ip",
								"size":  100,
							},
							"aggs": map[string]interface{}{
								"bytes": map[string]interface{}{
									"sum": map[string]interface{}{
										"field": "network.bytes",
									},
								},
							},
						},
					},
				},
			},
		}

		queryJSON, _ := json.Marshal(query)
		res, err := es.Search(
			es.Search.WithContext(context.Background()),
			es.Search.WithIndex("filebeat-*"),
			es.Search.WithBody(strings.NewReader(string(queryJSON))),
			es.Search.WithSize(0),
		)
		if err != nil {
			log.Fatalf("Error getting response: %s", err)
		}
		defer res.Body.Close()

		var result map[string]interface{}
		if err := json.NewDecoder(res.Body).Decode(&result); err != nil {
			log.Fatalf("Error parsing response: %s", err)
		}

		buckets := result["aggregations"].(map[string]interface{})["source_nodes"].(map[string]interface{})["buckets"].([]interface{})

		nodes := make(map[string]int)
		var names []string

		for _, bucket := range buckets {
			sourceIP := bucket.(map[string]interface{})["key"].(string)
			if _, exists := nodes[sourceIP]; !exists {
				nodes[sourceIP] = len(nodes)
				names = append(names, sourceIP)
			}

			destBuckets := bucket.(map[string]interface{})["destinations"].(map[string]interface{})["buckets"].([]interface{})
			for _, destBucket := range destBuckets {
				destIP := destBucket.(map[string]interface{})["key"].(string)
				if _, exists := nodes[destIP]; !exists {
					nodes[destIP] = len(nodes)
					names = append(names, destIP)
				}
			}
		}

		size := len(nodes)
		flow := make([][]float64, size)
		for i := range flow {
			flow[i] = make([]float64, size)
		}

		for _, bucket := range buckets {
			b := bucket.(map[string]interface{})
			sourceIP := b["key"].(string)
			sourceIdx := nodes[sourceIP]

			destBuckets := b["destinations"].(map[string]interface{})["buckets"].([]interface{})
			for _, destBucket := range destBuckets {
				d := destBucket.(map[string]interface{})
				destIP := d["key"].(string)
				bytes := d["bytes"].(map[string]interface{})["value"].(float64)

				destIdx := nodes[destIP]
				flow[sourceIdx][destIdx] = bytes
			}
		}

		p := plot.New()

		p.X.Min = -1
		p.X.Max = 1
		p.Y.Min = -1
		p.Y.Max = 1

		p.X.Label.Text = ""
		p.Y.Label.Text = ""
		p.X.Tick.Length = 0
		p.Y.Tick.Length = 0
		p.X.Tick.Label.Font.Size = 0
		p.Y.Tick.Label.Font.Size = 0
		p.X.LineStyle.Width = 0
		p.Y.LineStyle.Width = 0

		p.Title.Text = "Network Traffic Flow Between IPs"
		p.Title.TextStyle.Font.Size = vg.Points(16)
		p.Add(ChordDiagram{
			Flow:   flow,
			Labels: names,
			Color: func(i, j int) color.Color {
				return color.RGBA{R: uint8(30 * i), G: uint8(30 * j), B: 255, A: 200} // Increased base opacity
			},
		})

		if err := p.Save(24*vg.Inch, 24*vg.Inch, "network_flow.png"); err != nil {
			log.Fatalf("Error saving plot: %s", err)
		}
	}
}
