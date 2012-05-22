package main

import (
	"code.google.com/p/goprotobuf/proto"
	"OSMPBF"
	"os"
	"encoding/binary"
	"io"
	"bytes"
	"compress/zlib"
	"errors"
	"math"
	"flag"
	"runtime"
)

type blockData struct {
	blobHeader *OSMPBF.BlobHeader
	blobData []byte
}

type boundingBoxUpdate struct {
	wayIndex int
	lon float64
	lat float64
}

type node struct {
	id int64
	lon float64
	lat float64
	keys []string
	values []string
}

type way struct {
	id int64
	nodeIds []int64
	keys []string
	values []string
}

func readBlock(file io.Reader, size int32) ([]byte, error) {
	buffer := make([]byte, size)
	var idx int32 = 0
	for {
		cnt, err := file.Read(buffer[idx:])
		if err != nil {
			return nil, err
		}
		idx += int32(cnt)
		if idx == size {
			break
		}
	}
	return buffer, nil
}

func readNextBlobHeader(file *os.File) (*OSMPBF.BlobHeader, error) {
	var blobHeaderSize int32

	err := binary.Read(file, binary.BigEndian, &blobHeaderSize)
	if err != nil {
		return nil, err
	}

	if blobHeaderSize < 0 || blobHeaderSize > (64 * 1024 * 1024) {
		return nil, err
	}

	blobHeaderBytes, err := readBlock(file, blobHeaderSize)
	if err != nil {
		return nil, err
	}

	blobHeader := &OSMPBF.BlobHeader{}
	err = proto.Unmarshal(blobHeaderBytes, blobHeader)
	if err != nil {
		return nil, err
	}

	return blobHeader, nil
}

func decodeBlob(data blockData) ([]byte, error) {
	blob := &OSMPBF.Blob{}
	err := proto.Unmarshal(data.blobData, blob)
	if err != nil {
		return nil, err
	}

	var blobContent []byte
	if blob.Raw != nil {
		blobContent = blob.Raw
	} else if blob.ZlibData != nil {
		if blob.RawSize == nil {
			return nil, errors.New("decompressed size is required but not provided")
		}
		zlibBuffer := bytes.NewBuffer(blob.ZlibData)
		zlibReader, err := zlib.NewReader(zlibBuffer)
		if err != nil {
			return nil, err
		}
		blobContent, err = readBlock(zlibReader, *blob.RawSize)
		if err != nil {
			return nil, err
		}
		zlibReader.Close()
	} else {
		return nil, errors.New("Unsupported blob storage")
	}

	return blobContent, nil
}

func makePrimitiveBlockReader(file *os.File) chan blockData {
	retval := make(chan blockData)

	go func() {
		file.Seek(0, 0)
		for {
			blobHeader, err := readNextBlobHeader(file)
			if err == io.EOF {
				break
			} else if err != nil {
				println("Blob header read error:", err.Error())
				os.Exit(2)
			}

			blobBytes, err := readBlock(file, *blobHeader.Datasize)
			if err != nil {
				println("Blob read error:", err.Error())
				os.Exit(3)
			}

			retval <- blockData{ blobHeader, blobBytes }
		}
		close(retval)
	}()

	return retval
}

func supportedFilePass(file *os.File) {
	for data := range makePrimitiveBlockReader(file) {
		if *data.blobHeader.Type == "OSMHeader" {
			blockBytes, err := decodeBlob(data)
			if err != nil {
				println("OSMHeader blob read error:", err.Error())
				os.Exit(5)
			}

			header := &OSMPBF.HeaderBlock{}
			err = proto.Unmarshal(blockBytes, header)
			if err != nil {
				println("OSMHeader decode error:", err.Error())
				os.Exit(5)
			}

			for _, feat := range header.RequiredFeatures {
				if feat != "OsmSchema-V0.6" && feat != "DenseNodes" {
					println("Unsupported feature required in OSM header:", feat)
					os.Exit(5)
				}
			}
		}
	}
}

func findMatchingWaysPass(file *os.File, totalBlobCount int) [][]int64 {
	wayNodeRefs := make([][]int64, 0, 100)
	pending := make(chan bool)

	appendNodeRefs := make(chan []int64)
	appendNodeRefsComplete := make(chan bool)

	go func() {
		for nodeRefs := range appendNodeRefs {
			wayNodeRefs = append(wayNodeRefs, nodeRefs)
		}
		appendNodeRefsComplete <- true
	}()

	blockDataReader := makePrimitiveBlockReader(file)
	for i := 0; i < runtime.NumCPU() * 2; i++ {
		go func() {
			for data := range blockDataReader {
				if *data.blobHeader.Type == "OSMData" {
					blockBytes, err := decodeBlob(data)
					if err != nil {
						println("OSMData decode error:", err.Error())
						os.Exit(6)
					}

					primitiveBlock := &OSMPBF.PrimitiveBlock{}
					err = proto.Unmarshal(blockBytes, primitiveBlock)
					if err != nil {
						println("OSMData decode error:", err.Error())
						os.Exit(6)
					}

					for _, primitiveGroup := range primitiveBlock.Primitivegroup {
						for _, way := range primitiveGroup.Ways {
							for i, keyIndex := range way.Keys {
								valueIndex := way.Vals[i]
								key := string(primitiveBlock.Stringtable.S[keyIndex])
								value := string(primitiveBlock.Stringtable.S[valueIndex])
								if key == "leisure" && value == "golf_course" {
									var nodeRefs = make([]int64, len(way.Refs))
									var prevNodeId int64 = 0
									for index, deltaNodeId := range way.Refs {
										nodeId := prevNodeId + deltaNodeId
										prevNodeId = nodeId
										nodeRefs[index] = nodeId
									}
									appendNodeRefs <- nodeRefs
								}
							}
						}
					}
				}

				pending <- true
			}
		}()
	}

	blobCount := 0
	for _ = range pending {
		blobCount += 1
		if blobCount % 500 == 0 {
			println("\tComplete:", blobCount, "\tRemaining:", totalBlobCount - blobCount)
		}
		if blobCount == totalBlobCount {
			close(pending)
			close(appendNodeRefs)
			<-appendNodeRefsComplete
			close(appendNodeRefsComplete)
		}
	}

	return wayNodeRefs
}

func calculateLongLat(primitiveBlock *OSMPBF.PrimitiveBlock, rawlon int64, rawlat int64) (float64, float64){
	var lonOffset int64 = 0
	var latOffset int64 = 0
	var granularity int64 = 100
	if primitiveBlock.LonOffset != nil {
		lonOffset = *primitiveBlock.LonOffset
	}
	if primitiveBlock.LatOffset != nil {
		latOffset = *primitiveBlock.LatOffset
	}
	if primitiveBlock.Granularity != nil {
		granularity = int64(*primitiveBlock.Granularity)
	}

	lon := .000000001 * float64(lonOffset + (granularity * rawlon))
	lat := .000000001 * float64(latOffset + (granularity * rawlat))

	return lon, lat
}

func isInBoundingBoxes(boundingBoxes [][]float64, lon float64, lat float64) bool {
	for _, boundingBox := range boundingBoxes {
		if boundingBox == nil {
			continue
		}
		if lon >= boundingBox[0] && lat >= boundingBox[1] && lon <= boundingBox[2] && lat <= boundingBox[3] {
			return true
		}
	}
	return false
}

func calculateBoundingBoxesPass(file *os.File, wayNodeRefs [][]int64, totalBlobCount int) [][]float64 {

	// maps node ids to wayNodeRef indexes
	nodeOwners := make(map[int64][]int, len(wayNodeRefs) * 4)
	for wayIndex, way := range wayNodeRefs {
		for _, nodeId := range way {
			if nodeOwners[nodeId] == nil {
				nodeOwners[nodeId] = make([]int, 0, 1)
			}
			nodeOwners[nodeId] = append(nodeOwners[nodeId], wayIndex)
		}
	}

	pending := make(chan bool)
	updateWayBoundingBoxes := make(chan boundingBoxUpdate)
	updateWayBoundingBoxesComplete := make(chan bool)

	wayBoundingBoxes := make([][]float64, len(wayNodeRefs))

	go func() {
		for update := range updateWayBoundingBoxes {
			boundingBox := wayBoundingBoxes[update.wayIndex]
			if boundingBox == nil {
				boundingBox = make([]float64, 4)
				boundingBox[0] = update.lon
				boundingBox[1] = update.lat
				boundingBox[2] = update.lon
				boundingBox[3] = update.lat
				wayBoundingBoxes[update.wayIndex] = boundingBox
			} else {
				boundingBox[0] = math.Min(boundingBox[0], update.lon)
				boundingBox[1] = math.Min(boundingBox[1], update.lat)
				boundingBox[2] = math.Max(boundingBox[2], update.lon)
				boundingBox[3] = math.Max(boundingBox[3], update.lat)
			}
		}
		updateWayBoundingBoxesComplete <- true
	}()

	blockDataReader := makePrimitiveBlockReader(file)
	for i := 0; i < runtime.NumCPU() * 2; i++ {
		go func() {
			for data := range blockDataReader {
				if *data.blobHeader.Type == "OSMData" {
					blockBytes, err := decodeBlob(data)
					if err != nil {
						println("OSMData decode error:", err.Error())
						os.Exit(6)
					}

					primitiveBlock := &OSMPBF.PrimitiveBlock{}
					err = proto.Unmarshal(blockBytes, primitiveBlock)
					if err != nil {
						println("OSMData decode error:", err.Error())
						os.Exit(6)
					}

					for _, primitiveGroup := range primitiveBlock.Primitivegroup {
						for _, node := range primitiveGroup.Nodes {
							owners := nodeOwners[*node.Id]
							if owners == nil {
								continue
							}
							lon, lat := calculateLongLat(primitiveBlock, *node.Lon, *node.Lat)
							for _, wayIndex := range owners {
								updateWayBoundingBoxes <- boundingBoxUpdate{ wayIndex, lon, lat }
							}
						}

						if primitiveGroup.Dense != nil {
							var prevNodeId int64 = 0
							var prevLat int64 = 0
							var prevLon int64 = 0

							for idx, deltaNodeId := range primitiveGroup.Dense.Id {
								nodeId := prevNodeId + deltaNodeId
								rawlon := prevLon + primitiveGroup.Dense.Lon[idx]
								rawlat := prevLat + primitiveGroup.Dense.Lat[idx]

								prevNodeId = nodeId
								prevLon = rawlon
								prevLat = rawlat

								owners := nodeOwners[nodeId]
								if owners == nil {
									continue
								}
								lon, lat := calculateLongLat(primitiveBlock, rawlon, rawlat)
								for _, wayIndex := range owners {
									updateWayBoundingBoxes <- boundingBoxUpdate{ wayIndex, lon, lat }
								}
							}
						}
					}
				}

				pending <- true
			}
		}()
	}

	blobCount := 0
	for _ = range pending {
		blobCount += 1
		if blobCount % 500 == 0 {
			println("\tComplete:", blobCount, "\tRemaining:", totalBlobCount - blobCount)
		}
		if blobCount == totalBlobCount {
			close(pending)
			close(updateWayBoundingBoxes)
			<-updateWayBoundingBoxesComplete
			close(updateWayBoundingBoxesComplete)
		}
	}

	return wayBoundingBoxes
}

func findNodesWithinBoundingBoxesPass(file *os.File, boundingBoxes[][]float64, totalBlobCount int) []node {
	retvalNodes := make([]node, 0, 100000)
	pending := make(chan bool)

	appendNode := make(chan node)
	appendNodeComplete := make(chan bool)

	go func() {
		for node := range appendNode {
			retvalNodes = append(retvalNodes, node)
		}
		appendNodeComplete <- true
	}()

	blockDataReader := makePrimitiveBlockReader(file)
	for i := 0; i < runtime.NumCPU() * 2; i++ {
		go func() {
			for data := range blockDataReader {
				if *data.blobHeader.Type == "OSMData" {
					blockBytes, err := decodeBlob(data)
					if err != nil {
						println("OSMData decode error:", err.Error())
						os.Exit(6)
					}

					primitiveBlock := &OSMPBF.PrimitiveBlock{}
					err = proto.Unmarshal(blockBytes, primitiveBlock)
					if err != nil {
						println("OSMData decode error:", err.Error())
						os.Exit(6)
					}

					for _, primitiveGroup := range primitiveBlock.Primitivegroup {
						for _, osmNode := range primitiveGroup.Nodes {

							lon, lat := calculateLongLat(primitiveBlock, *osmNode.Lon, *osmNode.Lat)

							if isInBoundingBoxes(boundingBoxes, lon, lat) {
								keys := make([]string, len(osmNode.Keys))
								vals := make([]string, len(osmNode.Keys))
								for i, keyIndex := range osmNode.Keys {
									valueIndex := osmNode.Vals[i]
									keys[i] = string(primitiveBlock.Stringtable.S[keyIndex])
									vals[i] = string(primitiveBlock.Stringtable.S[valueIndex])
								}

								node := node{
									*osmNode.Id,
									lon,
									lat,
									keys,
									vals,
								}
								appendNode <- node
							}
						}

						if primitiveGroup.Dense != nil {
							var prevNodeId int64 = 0
							var prevLat int64 = 0
							var prevLon int64 = 0
							keyValIndex := 0

							for idx, deltaNodeId := range primitiveGroup.Dense.Id {
								nodeId := prevNodeId + deltaNodeId
								rawlon := prevLon + primitiveGroup.Dense.Lon[idx]
								rawlat := prevLat + primitiveGroup.Dense.Lat[idx]

								prevNodeId = nodeId
								prevLon = rawlon
								prevLat = rawlat

								startKeyValIndex := 0

								// Not sure why KeysVals can be length zero, this
								// doesn't seem to be documented, but I'll assume that
								// means none of the nodes have data associated with
								// them.
								if len(primitiveGroup.Dense.KeysVals) != 0 {
									startKeyValIndex = keyValIndex
									for primitiveGroup.Dense.KeysVals[keyValIndex] != 0 {
										keyValIndex += 2
									}
								}

								lon, lat := calculateLongLat(primitiveBlock, rawlon, rawlat)
								if isInBoundingBoxes(boundingBoxes, lon, lat) {
									numItems := 0
									if len(primitiveGroup.Dense.KeysVals) != 0 {
										numItems = (keyValIndex - startKeyValIndex) / 2
									}
									keys := make([]string, numItems)
									vals := make([]string, numItems)
									for i := 0; i < numItems; i++ {
										keys[i] = string(primitiveBlock.Stringtable.S[primitiveGroup.Dense.KeysVals[startKeyValIndex + (i * 2)]])
										vals[i] = string(primitiveBlock.Stringtable.S[primitiveGroup.Dense.KeysVals[startKeyValIndex + (i * 2) + 1]])
									}

									node := node{
										nodeId,
										lon,
										lat,
										keys,
										vals,
									}
									appendNode <- node
								}

								keyValIndex += 1
							}
						}
					}
				}

				pending <- true
			}
		}()
	}

	blobCount := 0
	for _ = range pending {
		blobCount += 1
		if blobCount % 500 == 0 {
			println("\tComplete:", blobCount, "\tRemaining:", totalBlobCount - blobCount)
		}
		if blobCount == totalBlobCount {
			close(pending)
			close(appendNode)
			<-appendNodeComplete
			close(appendNodeComplete)
		}
	}

	return retvalNodes
}

func findWaysUsingNodesPass(file *os.File, nodes []node, totalBlobCount int) []way {
	ways := make([]way, 0, 1000)
	pending := make(chan bool)

	nodeSet := make(map[int64]bool, len(nodes))
	for _, node := range nodes {
		nodeSet[node.id] = true
	}

	appendWay := make(chan way)
	appendWayComplete := make(chan bool)

	go func() {
		for way := range appendWay {
			ways = append(ways, way)
		}
		appendWayComplete <- true
	}()

	blockDataReader := makePrimitiveBlockReader(file)
	for i := 0; i < runtime.NumCPU() * 2; i++ {
		go func() {
			for data := range blockDataReader {
				if *data.blobHeader.Type == "OSMData" {
					blockBytes, err := decodeBlob(data)
					if err != nil {
						println("OSMData decode error:", err.Error())
						os.Exit(6)
					}

					primitiveBlock := &OSMPBF.PrimitiveBlock{}
					err = proto.Unmarshal(blockBytes, primitiveBlock)
					if err != nil {
						println("OSMData decode error:", err.Error())
						os.Exit(6)
					}

					for _, primitiveGroup := range primitiveBlock.Primitivegroup {
						for _, osmWay := range primitiveGroup.Ways {

							match := false

							var prevNodeId int64 = 0
							for _, deltaNodeId := range osmWay.Refs {
								nodeId := prevNodeId + deltaNodeId
								prevNodeId = nodeId

								if nodeSet[nodeId] {
									match = true
									break
								}
							}

							if match {
								nodeRefs := make([]int64, len(osmWay.Refs))
								prevNodeId = 0
								for index, deltaNodeId := range osmWay.Refs {
									nodeId := prevNodeId + deltaNodeId
									prevNodeId = nodeId
									nodeRefs[index] = nodeId
								}

								keys := make([]string, len(osmWay.Keys))
								vals := make([]string, len(osmWay.Keys))
								for i, keyIndex := range osmWay.Keys {
									valueIndex := osmWay.Vals[i]
									keys[i] = string(primitiveBlock.Stringtable.S[keyIndex])
									vals[i] = string(primitiveBlock.Stringtable.S[valueIndex])
								}

								appendWay <- way{
									*osmWay.Id,
									nodeRefs,
									keys,
									vals,
								}
							}
						}
					}
				}

				pending <- true
			}
		}()
	}

	blobCount := 0
	for _ = range pending {
		blobCount += 1
		if blobCount % 500 == 0 {
			println("\tComplete:", blobCount, "\tRemaining:", totalBlobCount - blobCount)
		}
		if blobCount == totalBlobCount {
			close(pending)
			close(appendWay)
			<-appendWayComplete
			close(appendWayComplete)
		}
	}

	return ways
}

func writeBlock(file *os.File, block interface{}, blockType string) error {
	blobContent, err := proto.Marshal(block)
	if err != nil {
		return err
	}

	var blobContentLength int32 = int32(len(blobContent))

	blob := OSMPBF.Blob{}
	blob.Raw = blobContent
	blob.RawSize = &blobContentLength
	blobBytes, err := proto.Marshal(&blob)
	if err != nil {
		return err
	}

	var blobBytesLength int32 = int32(len(blobBytes))

	blobHeader := OSMPBF.BlobHeader{}
	blobHeader.Type = &blockType
	blobHeader.Datasize = &blobBytesLength
	blobHeaderBytes, err := proto.Marshal(&blobHeader)
	if err != nil {
		return err
	}

	var blobHeaderLength int32 = int32(len(blobHeaderBytes))

	err = binary.Write(file, binary.BigEndian, blobHeaderLength)
	if err != nil {
		return err
	}
	_, err = file.Write(blobHeaderBytes)
	if err != nil {
		return err
	}
	_, err = file.Write(blobBytes)
	if err != nil {
		return err
	}

	return nil
}

func writeHeader(file *os.File) error {
	writingProgram := "go thingy"
	header := OSMPBF.HeaderBlock{}
	header.Writingprogram = &writingProgram
	header.RequiredFeatures = []string{ "OsmSchema-V0.6" }
	return writeBlock(file, &header, "OSMHeader")
}

func writeNodes(file *os.File, nodes []node) error {
	if len(nodes) == 0 {
		return nil
	}

	for nodeGroupIndex := 0; nodeGroupIndex < (len(nodes) / 8000) + 1; nodeGroupIndex++ {
		beg := (nodeGroupIndex + 0) * 8000
		end := (nodeGroupIndex + 1) * 8000
		if len(nodes) < end {
			end = len(nodes)
		}
		nodeGroup := nodes[beg:end]

		stringTable := make([][]byte, 1, 1000)
		stringTableIndexes := make(map[string]uint32, 0)

		for _, node := range nodeGroup {
			for _, s := range node.keys {
				idx := stringTableIndexes[s]
				if idx == 0 {
					stringTableIndexes[s] = uint32(len(stringTable))
					stringTable = append(stringTable, []byte(s))
				}
			}
			for _, s := range node.values {
				idx := stringTableIndexes[s]
				if idx == 0 {
					stringTableIndexes[s] = uint32(len(stringTable))
					stringTable = append(stringTable, []byte(s))
				}
			}
		}

		osmNodes := make([]*OSMPBF.Node, len(nodeGroup))

		for idx, node := range nodeGroup {
			osmNode := &OSMPBF.Node{}

			var nodeId int64 = node.id
			osmNode.Id = &nodeId

			var rawlon int64 = int64(node.lon / .000000001) / 100
			var rawlat int64 = int64(node.lat / .000000001) / 100
			osmNode.Lon = &rawlon
			osmNode.Lat = &rawlat

			osmNode.Keys = make([]uint32, len(node.keys))
			for i, s := range node.keys {
				osmNode.Keys[i] = stringTableIndexes[s]
			}
			osmNode.Vals = make([]uint32, len(node.values))
			for i, s := range node.values {
				osmNode.Vals[i] = stringTableIndexes[s]
			}
			osmNodes[idx] = osmNode
		}

		group := OSMPBF.PrimitiveGroup{}
		group.Nodes = osmNodes

		block := OSMPBF.PrimitiveBlock{}
		block.Stringtable = &OSMPBF.StringTable { stringTable, nil }
		block.Primitivegroup = []*OSMPBF.PrimitiveGroup{ &group }
		err := writeBlock(file, &block, "OSMData")
		if err != nil {
			return err
		}
	}

	return nil
}

func writeWays(file *os.File, ways []way) error {
	if len(ways) == 0 {
		return nil
	}

	for wayGroupIndex := 0; wayGroupIndex < (len(ways) / 8000) + 1; wayGroupIndex++ {
		beg := (wayGroupIndex + 0) * 8000
		end := (wayGroupIndex + 1) * 8000
		if len(ways) < end {
			end = len(ways)
		}
		wayGroup := ways[beg:end]

		stringTable := make([][]byte, 1, 1000)
		stringTableIndexes := make(map[string]uint32, 0)

		for _, way := range wayGroup {
			for _, s := range way.keys {
				idx := stringTableIndexes[s]
				if idx == 0 {
					stringTableIndexes[s] = uint32(len(stringTable))
					stringTable = append(stringTable, []byte(s))
				}
			}
			for _, s := range way.values {
				idx := stringTableIndexes[s]
				if idx == 0 {
					stringTableIndexes[s] = uint32(len(stringTable))
					stringTable = append(stringTable, []byte(s))
				}
			}
		}

		osmWays := make([]*OSMPBF.Way, len(wayGroup))

		for idx, way := range wayGroup {
			osmWay := &OSMPBF.Way{}

			var wayId int64 = way.id
			osmWay.Id = &wayId

			// delta-encode the node ids
			nodeRefs := make([]int64, len(way.nodeIds))
			var prevNodeId int64 = 0
			for i, nodeId := range(way.nodeIds) {
				nodeIdDelta := nodeId - prevNodeId
				prevNodeId = nodeId
				nodeRefs[i] = nodeIdDelta
			}
			osmWay.Refs = nodeRefs

			osmWay.Keys = make([]uint32, len(way.keys))
			for i, s := range way.keys {
				osmWay.Keys[i] = stringTableIndexes[s]
			}
			osmWay.Vals = make([]uint32, len(way.values))
			for i, s := range way.values {
				osmWay.Vals[i] = stringTableIndexes[s]
			}
			osmWays[idx] = osmWay
		}

		group := OSMPBF.PrimitiveGroup{}
		group.Ways = osmWays

		block := OSMPBF.PrimitiveBlock{}
		block.Stringtable = &OSMPBF.StringTable { stringTable, nil }
		block.Primitivegroup = []*OSMPBF.PrimitiveGroup{ &group }
		err := writeBlock(file, &block, "OSMData")
		if err != nil {
			return err
		}
	}

	return nil
}

func main() {
	runtime.GOMAXPROCS(runtime.NumCPU() * 2)

	flag.Parse()
	fname := flag.Arg(0)
	file, err := os.Open(fname)
	if err != nil {
		println("Unable to open file:", err.Error())
		os.Exit(1)
	}

	// Count the total number of blobs; provides a nice progress indicator
	totalBlobCount := 0
	for {
		blobHeader, err := readNextBlobHeader(file)
		if err == io.EOF {
			break
		} else if err != nil {
			println("Blob header read error:", err.Error())
			os.Exit(2)
		}

		totalBlobCount += 1
		file.Seek(int64(*blobHeader.Datasize), 1)
	}
	println("Total number of blobs:", totalBlobCount)

	println("Pass 1/5: Find OSMHeaders")
	supportedFilePass(file)
	println("Pass 1/5: Complete")

	println("Pass 2/5: Find node references of matching areas")
	wayNodeRefs := findMatchingWaysPass(file, totalBlobCount)
	println("Pass 2/5: Complete;", len(wayNodeRefs), "matching ways found.")

	println("Pass 3/5: Establish bounding boxes")
	boundingBoxes := calculateBoundingBoxesPass(file, wayNodeRefs, totalBlobCount)
	println("Pass 3/5: Complete;", len(boundingBoxes), "bounding boxes calculated.")

	println("Pass 4/5: Find nodes within bounding boxes")
	nodes := findNodesWithinBoundingBoxesPass(file, boundingBoxes, totalBlobCount)
	println("Pass 4/5: Complete;", len(nodes), "nodes located.")

	println("Pass 5/5: Find ways using intersecting nodes")
	ways := findWaysUsingNodesPass(file, nodes, totalBlobCount)
	println("Pass 5/5: Complete;", len(ways), "ways located.")

	output, err := os.OpenFile("output.osm.pbf", os.O_CREATE | os.O_WRONLY | os.O_TRUNC, 0664)
	if err != nil {
		println("Output file write error:", err.Error())
		os.Exit(2)
	}

	println("Out 1/3: Writing header")
	err = writeHeader(output)
	if err != nil {
		println("Output file write error:", err.Error())
		os.Exit(2)
	}

	println("Out 2/3: Writing nodes")
	err = writeNodes(output, nodes)
	if err != nil {
		println("Output file write error:", err.Error())
		os.Exit(2)
	}

	println("Out 3/3: Writing ways")
	err = writeWays(output, ways)
	if err != nil {
		println("Output file write error:", err.Error())
		os.Exit(2)
	}

	err = output.Close()
	if err != nil {
		println("Output file write error:", err.Error())
		os.Exit(2)
	}
}
