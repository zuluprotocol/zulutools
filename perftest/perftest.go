package perftest

import (
	"bufio"
	"flag"
	"fmt"
	"log"
	"math/rand"
	"os"
	"strconv"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	datanode "code.vegaprotocol.io/vega/protos/data-node/api/v2"
	proto "code.vegaprotocol.io/vega/protos/vega"
	commandspb "code.vegaprotocol.io/vega/protos/vega/commands/v1"
)

// Opts hold the command line values
type Opts struct {
	DataNodeAddr      string
	WalletURL         string
	FaucetURL         string
	GanacheURL        string
	TokenKeysFile     string
	CommandsPerSecond int
	RuntimeSeconds    int
	UserCount         int
	MarketCount       int
	Voters            int
	MoveMid           bool
	LPOrdersPerSide   int
	BatchSize         int
	PeggedOrders      int
	PriceLevels       int
	StartingMidPrice  int64
	FillPriceLevels   bool
}

type perfLoadTesting struct {
	// Information about all the users we can use to send orders
	users []UserDetails

	dataNode dnWrapper

	wallet walletWrapper
}

func (p *perfLoadTesting) connectToDataNode(dataNodeAddr string) (map[string]string, error) {
	connection, err := grpc.Dial(dataNodeAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		// Something went wrong
		return nil, fmt.Errorf("failed to connect to the datanode gRPC port: %w ", err)
	}

	dn := datanode.NewTradingDataServiceClient(connection)

	p.dataNode = dnWrapper{dataNode: dn,
		wallet: p.wallet}

	// load in all the assets
	for {
		assets, err := p.dataNode.getAssets()
		if err != nil {
			return nil, err
		}
		if len(assets) != 0 {
			return assets, nil
		}
		time.Sleep(time.Second * 1)
	}
}

func (p *perfLoadTesting) LoadUsers(tokenFilePath string, userCount int) error {
	// See if we have a token file defined and if so load all the wallet names and api-keys
	p.users = []UserDetails{}
	tokenFile, err := os.Open(tokenFilePath)
	if err != nil {
		return err
	}
	fileScanner := bufio.NewScanner(tokenFile)
	fileScanner.Split(bufio.ScanLines)

	for fileScanner.Scan() {
		lineParts := strings.Split(fileScanner.Text(), " ")
		if len(lineParts) == 2 {
			pubKey, _ := p.wallet.GetFirstKey(lineParts[1])

			p.users = append(p.users, UserDetails{userName: lineParts[0], token: lineParts[1], pubKey: pubKey})
		}
		if len(p.users) == userCount {
			break
		}
	}
	return nil
}

func (p *perfLoadTesting) depositTokens(assets map[string]string, faucetURL, ganacheURL string, voters int) error {
	if len(ganacheURL) > 0 {
		for index, user := range p.users {
			if index >= voters {
				break
			}
			sendVegaTokens(user.pubKey, ganacheURL)
			time.Sleep(time.Second * 1)
		}
	}

	// If the first user has not tokens, top everyone up
	// quickly without checking if they need it
	asset := assets["fUSDC"]
	amount, _ := p.dataNode.getAssetsPerUser(p.users[0].pubKey, asset)
	if amount == 0 {
		for t := 0; t < 50; t++ {
			for _, user := range p.users {
				err := topUpAsset(faucetURL, user.pubKey, asset, 100000000)
				if err != nil {
					return err
				}
				time.Sleep(time.Millisecond * 5)
			}
		}

		// Add some more to the special accounts as we might need it for price level orders
		for t := 0; t < 100; t++ {
			err := topUpAsset(faucetURL, p.users[0].pubKey, asset, 100000000)
			if err != nil {
				return err
			}
			time.Sleep(time.Millisecond * 5)
			err = topUpAsset(faucetURL, p.users[1].pubKey, asset, 100000000)
			if err != nil {
				return err
			}
			time.Sleep(time.Millisecond * 5)
		}

	}
	time.Sleep(time.Second * 5)

	for _, user := range p.users {
		for amount, _ = p.dataNode.getAssetsPerUser(user.pubKey, asset); amount < 5000000000; {
			err := topUpAsset(faucetURL, user.pubKey, asset, 100000000)
			if err != nil {
				return nil
			}
			time.Sleep(time.Second * 1)
			amount, _ = p.dataNode.getAssetsPerUser(user.pubKey, asset)
		}
	}
	return nil
}

func (p *perfLoadTesting) checkNetworkLimits(opts Opts) error {
	// Check the limit of the number of orders per side in the LP shape
	networkParam, err := p.dataNode.getNetworkParam("market.liquidityProvision.shapes.maxSize")
	if err != nil {
		fmt.Println("Failed to get LP maximum shape size")
		return err
	}
	maxLPShape, _ := strconv.ParseInt(networkParam, 0, 32)

	if opts.LPOrdersPerSide > int(maxLPShape) {
		return fmt.Errorf("supplied lp size greater than network param (%d>%d)", opts.LPOrdersPerSide, maxLPShape)
	}

	// Check the maximum number of orders in a batch
	networkParam, err = p.dataNode.getNetworkParam("spam.protection.max.batchSize")
	if err != nil {
		fmt.Println("Failed to get maximum order batch size")
		return err
	}
	maxBatchSize, _ := strconv.ParseInt(networkParam, 0, 32)

	if opts.BatchSize > int(maxBatchSize) {
		return fmt.Errorf("supplied order batch size is greater than network param (%d>%d)", opts.BatchSize, maxBatchSize)
	}
	return nil
}

func (p *perfLoadTesting) displayKeyUsers() {
	fmt.Println("Special user 1:", p.users[0].pubKey)
	fmt.Println("Special user 2:", p.users[1].pubKey)
}

func (p *perfLoadTesting) proposeAndEnactMarket(numberOfMarkets, voters, maxLPShape int, startingMidPrice int64) ([]string, error) {
	markets := p.dataNode.getMarkets()
	if len(markets) == 0 {
		for i := 0; i < numberOfMarkets; i++ {
			err := p.wallet.NewMarket(i, p.users[0])
			if err != nil {
				return nil, err
			}
			time.Sleep(time.Second * 7)
			propID, err := p.dataNode.getPendingProposalID()
			if err != nil {
				return nil, err
			}
			err = p.dataNode.voteOnProposal(p.users, propID, voters)
			if err != nil {
				return nil, err
			}
			// We have to wait for the market to be enacted
			err = p.dataNode.waitForMarketEnactment(propID, 40)
			if err != nil {
				return nil, err
			}
		}
	}
	time.Sleep(time.Second * 6)

	// Move markets out of auction
	markets = p.dataNode.getMarkets()
	marketIds := []string{}
	if len(markets) >= numberOfMarkets {
		for _, market := range markets {
			marketIds = append(marketIds, market.Id)
			if market.State != proto.Market_STATE_ACTIVE {
				// Send in a liquidity provision so we can get the market out of auction
				for j := 0; j < voters; j++ {
					p.wallet.SendLiquidityProvision(p.users[j], market.Id, maxLPShape)
				}
				p.wallet.SendOrder(p.users[0], &commandspb.OrderSubmission{MarketId: market.Id,
					Price:       fmt.Sprint(startingMidPrice + 100),
					Size:        100,
					Side:        proto.Side_SIDE_SELL,
					Type:        proto.Order_TYPE_LIMIT,
					TimeInForce: proto.Order_TIME_IN_FORCE_GTC})
				p.wallet.SendOrder(p.users[1], &commandspb.OrderSubmission{MarketId: market.Id,
					Price:       fmt.Sprint(startingMidPrice - 100),
					Size:        100,
					Side:        proto.Side_SIDE_BUY,
					Type:        proto.Order_TYPE_LIMIT,
					TimeInForce: proto.Order_TIME_IN_FORCE_GTC})
				p.wallet.SendOrder(p.users[0], &commandspb.OrderSubmission{MarketId: market.Id,
					Price:       fmt.Sprint(startingMidPrice),
					Size:        5,
					Side:        proto.Side_SIDE_BUY,
					Type:        proto.Order_TYPE_LIMIT,
					TimeInForce: proto.Order_TIME_IN_FORCE_GTC})
				p.wallet.SendOrder(p.users[1], &commandspb.OrderSubmission{MarketId: market.Id,
					Price:       fmt.Sprint(startingMidPrice),
					Size:        5,
					Side:        proto.Side_SIDE_SELL,
					Type:        proto.Order_TYPE_LIMIT,
					TimeInForce: proto.Order_TIME_IN_FORCE_GTC})
			}
		}
	} else {
		return nil, fmt.Errorf("failed to get open market")
	}
	time.Sleep(time.Second * 5)
	return marketIds, nil
}

func (p *perfLoadTesting) seedPeggedOrders(marketIDs []string, peggedOrderCount, priceLevels int) error {
	// Loop through every market
	for _, marketID := range marketIDs {
		for i := 0; i < peggedOrderCount; i++ {
			// Only use the first 2 users as they won't have their orders deleted
			userOffset := rand.Intn(2)
			user := p.users[userOffset]

			priceOffset := priceLevels + rand.Intn(100)
			side := rand.Intn(100)

			order := &commandspb.OrderSubmission{
				MarketId:    marketID,
				Size:        1,
				Type:        proto.Order_TYPE_LIMIT,
				TimeInForce: proto.Order_TIME_IN_FORCE_GTC,
				PeggedOrder: &proto.PeggedOrder{
					Offset: fmt.Sprint(priceOffset),
				},
			}

			if side < 50 {
				// We are a buy side pegged order
				order.Side = proto.Side_SIDE_BUY
				if side%2 == 0 {
					order.PeggedOrder.Reference = proto.PeggedReference_PEGGED_REFERENCE_BEST_BID
				} else {
					order.PeggedOrder.Reference = proto.PeggedReference_PEGGED_REFERENCE_MID
				}
			} else {
				// We are a sell side pegged order
				order.Side = proto.Side_SIDE_SELL
				if side%2 == 0 {
					order.PeggedOrder.Reference = proto.PeggedReference_PEGGED_REFERENCE_BEST_ASK
				} else {
					order.PeggedOrder.Reference = proto.PeggedReference_PEGGED_REFERENCE_MID
				}
			}
			err := p.wallet.SendOrder(user, order)
			if err != nil {
				return err
			}
			time.Sleep(time.Millisecond * 25)
		}
	}
	return nil
}

func (p *perfLoadTesting) seedPriceLevels(marketIDs []string, users, priceLevels int, startingMidPrice int64) error {
	for _, marketID := range marketIDs {
		// Buys first
		for i := startingMidPrice - 1; i > startingMidPrice-int64(priceLevels); i-- {
			user := p.users[rand.Intn(2)]
			err := p.wallet.SendOrder(user, &commandspb.OrderSubmission{MarketId: marketID,
				Price:       fmt.Sprint(i),
				Size:        1,
				Side:        proto.Side_SIDE_BUY,
				Type:        proto.Order_TYPE_LIMIT,
				TimeInForce: proto.Order_TIME_IN_FORCE_GTC})
			if err != nil {
				log.Println("Failed to send price level buy order", err)
				return err
			}
			time.Sleep(time.Millisecond * 25)
		}
		// Now the sells
		for i := startingMidPrice; i <= startingMidPrice+int64(priceLevels); i++ {
			user := p.users[rand.Intn(2)]
			err := p.wallet.SendOrder(user, &commandspb.OrderSubmission{MarketId: marketID,
				Price:       fmt.Sprint(i),
				Size:        1,
				Side:        proto.Side_SIDE_SELL,
				Type:        proto.Order_TYPE_LIMIT,
				TimeInForce: proto.Order_TIME_IN_FORCE_GTC})
			if err != nil {
				log.Println("Failed to send price level buy order", err)
				return err
			}
			time.Sleep(time.Millisecond * 25)
		}
	}
	return nil
}

func (p *perfLoadTesting) sendTradingLoad(marketIDs []string, users, ops, runTimeSeconds, priceLevels int, startingMidPrice int64, moveMid bool) error {
	// Start load testing by sending off lots of orders at a given rate
	userCount := users - 2
	now := time.Now()
	midPrice := startingMidPrice
	transactionCount := 0
	delays := 0
	transactionsPerSecond := ops
	opsScale := 1.0
	if transactionsPerSecond > 1 {
		opsScale = float64(transactionsPerSecond - 1)
	}
	// Work out how many transactions we need for the length of the run
	numberOfTransactions := runTimeSeconds * transactionsPerSecond
	for i := 0; i < numberOfTransactions; i++ {
		// Pick a random market to send the trade on
		marketID := marketIDs[rand.Intn(len(marketIDs))]
		userOffset := rand.Intn(userCount) + 2
		user := p.users[userOffset]
		choice := rand.Intn(100)
		if choice < 3 {
			// Perform a cancel all
			err := p.wallet.SendCancelAll(user, marketID)
			if err != nil {
				log.Println("Failed to send cancel all", err)
			}

			if moveMid {
				// Move the midprice around as well
				midPrice = midPrice + (rand.Int63n(3) - 1)
				if midPrice < startingMidPrice-500 {
					midPrice = startingMidPrice - 495
				}
				if midPrice > startingMidPrice+500 {
					midPrice = startingMidPrice + 495
				}
			}
		} else if choice < 10 {
			// Perform a market order to generate some trades
			if choice%2 == 1 {
				err := p.wallet.SendOrder(user, &commandspb.OrderSubmission{MarketId: marketID,
					Size:        3,
					Side:        proto.Side_SIDE_BUY,
					Type:        proto.Order_TYPE_MARKET,
					TimeInForce: proto.Order_TIME_IN_FORCE_IOC,
					Reference:   "MarketBuy"})
				if err != nil {
					log.Println("Failed to send market buy order", err)
				}
			} else {
				err := p.wallet.SendOrder(user, &commandspb.OrderSubmission{MarketId: marketID,
					Size:        3,
					Side:        proto.Side_SIDE_SELL,
					Type:        proto.Order_TYPE_MARKET,
					TimeInForce: proto.Order_TIME_IN_FORCE_IOC,
					Reference:   "MarketSell"})
				if err != nil {
					log.Println("Failed to send market sell order", err)
				}
			}
		} else {
			// Insert a new order to fill up the book
			priceOffset := rand.Int63n(int64(priceLevels*2)) - int64(priceLevels)
			if priceOffset > 0 {
				// Send a sell
				err := p.wallet.SendOrder(user, &commandspb.OrderSubmission{MarketId: marketID,
					Price:       fmt.Sprint((midPrice - 1) + priceOffset),
					Size:        1,
					Side:        proto.Side_SIDE_SELL,
					Type:        proto.Order_TYPE_LIMIT,
					TimeInForce: proto.Order_TIME_IN_FORCE_GTC,
					Reference:   "NonTouchingLimitSell"})
				if err != nil {
					log.Println("Failed to send non crossing random limit sell order", err)
				}
			} else {
				// Send a buy
				err := p.wallet.SendOrder(user, &commandspb.OrderSubmission{MarketId: marketID,
					Price:       fmt.Sprint(midPrice + priceOffset),
					Size:        1,
					Side:        proto.Side_SIDE_BUY,
					Type:        proto.Order_TYPE_LIMIT,
					TimeInForce: proto.Order_TIME_IN_FORCE_GTC,
					Reference:   "NonTouchingLimitBuy"})
				if err != nil {
					log.Println("Failed to send non crossing random limit buy order", err)
				}
			}
		}
		transactionCount++

		actualDiffSeconds := time.Since(now).Seconds()
		wantedDiffSeconds := float64(transactionCount) / opsScale

		// See if we are sending quicker than we should
		if actualDiffSeconds < wantedDiffSeconds {
			delayMillis := (wantedDiffSeconds - actualDiffSeconds) * 1000
			if delayMillis > 10 {
				time.Sleep(time.Millisecond * time.Duration(delayMillis))
				delays++
			}
			actualDiffSeconds = time.Since(now).Seconds()
		}

		if actualDiffSeconds >= 1 {
			fmt.Printf("\rSending load transactions...[%d/%d] %dcps  ", i, numberOfTransactions, transactionCount)
			transactionCount = 0
			delays = 0
			now = time.Now()
		}
	}
	fmt.Printf("\rSending load transactions...")
	return nil
}

func (p *perfLoadTesting) sendBatchTradingLoad(marketIDs []string, users, ops, runTimeSeconds, batchSize, priceLevels int, startingMidPrice int64, moveMid bool) error {
	userCount := users - 2
	now := time.Now()
	midPrice := startingMidPrice
	transactionCount := 0
	batchCount := 0
	transactionsPerSecond := ops

	// Map to store the batch orders in
	batchOrders := map[int]*BatchOrders{}

	// Work out how many transactions we need for the length of the run
	numberOfTransactions := runTimeSeconds * transactionsPerSecond
	for i := 0; i < numberOfTransactions; i++ {
		// Pick a random market to send the trade on
		marketID := marketIDs[rand.Intn(len(marketIDs))]
		userOffset := rand.Intn(userCount) + 2
		user := p.users[userOffset]

		batch := batchOrders[userOffset]
		if batch == nil {
			batch = &BatchOrders{}
			batchOrders[userOffset] = batch
		}

		choice := rand.Intn(100)
		if choice < 3 {
			// Perform a cancel all
			batch.cancels = append(batch.cancels, &commandspb.OrderCancellation{MarketId: marketID})

			if moveMid {
				// Move the midprice around as well
				midPrice = midPrice + (rand.Int63n(3) - 1)
				if midPrice < startingMidPrice-500 {
					midPrice = startingMidPrice - 495
				}
				if midPrice > startingMidPrice+500 {
					midPrice = startingMidPrice + 495
				}
			}
		} else if choice < 10 {
			// Perform a market order to generate some trades
			if choice%2 == 1 {
				batch.orders = append(batch.orders, &commandspb.OrderSubmission{MarketId: marketID,
					Size:        3,
					Side:        proto.Side_SIDE_BUY,
					Type:        proto.Order_TYPE_MARKET,
					TimeInForce: proto.Order_TIME_IN_FORCE_IOC})
			} else {
				batch.orders = append(batch.orders, &commandspb.OrderSubmission{MarketId: marketID,
					Size:        3,
					Side:        proto.Side_SIDE_SELL,
					Type:        proto.Order_TYPE_MARKET,
					TimeInForce: proto.Order_TIME_IN_FORCE_IOC})
			}
		} else {
			// Insert a new order to fill up the book
			priceOffset := rand.Int63n(int64(priceLevels*2)) - int64(priceLevels)
			if priceOffset > 0 {
				// Send a sell
				batch.orders = append(batch.orders, &commandspb.OrderSubmission{MarketId: marketID,
					Price:       fmt.Sprint((midPrice - 1) + priceOffset),
					Size:        1,
					Side:        proto.Side_SIDE_SELL,
					Type:        proto.Order_TYPE_LIMIT,
					TimeInForce: proto.Order_TIME_IN_FORCE_GTC})
			} else {
				// Send a buy
				batch.orders = append(batch.orders, &commandspb.OrderSubmission{MarketId: marketID,
					Price:       fmt.Sprint(midPrice + priceOffset),
					Size:        1,
					Side:        proto.Side_SIDE_BUY,
					Type:        proto.Order_TYPE_LIMIT,
					TimeInForce: proto.Order_TIME_IN_FORCE_GTC})
			}
		}
		transactionCount++

		// If this batch has reached it's limit, send it and reset
		if batch.GetMessageCount() == batchSize {
			err := p.wallet.SendBatchOrders(user, batch.cancels, batch.amends, batch.orders)
			if err != nil {
				return err
			}
			batchCount++
			batch.Empty()
		}

		// If we have done enough orders for this second, send them off
		if transactionCount == ops {
			for userOff, value := range batchOrders {
				if value.GetMessageCount() > 0 {
					err := p.wallet.SendBatchOrders(p.users[userOff], value.cancels, value.amends, value.orders)
					if err != nil {
						return err
					}
					batchCount++
					value.Empty()
				}
			}

			// If we are still under 1 second, wait before moving on to the next set of orders
			timeUsed := time.Since(now).Seconds()

			// Add in a delay to keep us processing at a per second rate
			if timeUsed < 1.0 {
				milliSecondsLeft := int((1.0 - timeUsed) * 1000.0)
				time.Sleep(time.Millisecond * time.Duration(milliSecondsLeft))
			}
			fmt.Printf("\rSending load transactions...[%d/%d] %dcps %dbps ", i, numberOfTransactions, transactionCount, batchCount)
			transactionCount = 0
			batchCount = 0
			now = time.Now()
		}
	}
	fmt.Printf("\rSending load transactions...")
	return nil
}

// Run is the main function of `perftest` package
func Run(opts Opts) error {
	flag.Parse()

	plt := perfLoadTesting{wallet: walletWrapper{walletURL: opts.WalletURL}}

	fmt.Print("Connecting to data node...")
	if len(opts.DataNodeAddr) <= 0 {
		fmt.Println("FAILED")
		return fmt.Errorf("error: missing datanode grpc server address")
	}

	// Connect to data node and check it's working
	assets, err := plt.connectToDataNode(opts.DataNodeAddr)
	if err != nil {
		fmt.Println("FAILED")
		return err
	}
	fmt.Println("Complete")

	// Create a set of users
	fmt.Print("Loading users from token API file...")
	if len(opts.TokenKeysFile) > 0 {
		err = plt.LoadUsers(opts.TokenKeysFile, opts.UserCount)
		if err != nil {
			fmt.Println("FAILED")
			return err
		}
	} else {
		fmt.Println("FAILED")
		return fmt.Errorf("error: unable to open token file")
	}
	fmt.Println("Complete")

	// Dump out the users we are using for special orders
	plt.displayKeyUsers()

	// Send some tokens to any newly created users
	fmt.Print("Depositing tokens and assets...")
	err = plt.depositTokens(assets, opts.FaucetURL, opts.GanacheURL, opts.Voters)
	if err != nil {
		fmt.Println("FAILED")
		return err
	}
	fmt.Println("Complete")

	err = plt.checkNetworkLimits(opts)
	if err != nil {
		return err
	}

	// Send in a proposal to create a new market and vote to get it through
	fmt.Print("Proposing and voting in new market...")
	marketIDs, err := plt.proposeAndEnactMarket(opts.MarketCount, opts.Voters, opts.LPOrdersPerSide, opts.StartingMidPrice)
	if err != nil {
		fmt.Println("FAILED")
		return err
	}
	fmt.Println("Complete")

	// Do we need to seed the market with some pegged orders for load testing purposes?
	if opts.PeggedOrders > 0 {
		fmt.Print("Sending pegged orders to market...")
		err = plt.seedPeggedOrders(marketIDs, opts.PeggedOrders, opts.PriceLevels)
		if err != nil {
			fmt.Println("FAILED")
			return err
		}
		fmt.Println("Complete")
	}

	// Lets place an order at every possible price level
	if opts.FillPriceLevels {
		fmt.Print("Adding an order to every price level in each market...")
		err = plt.seedPriceLevels(marketIDs, opts.UserCount, opts.PriceLevels, opts.StartingMidPrice)
		if err != nil {
			fmt.Println("FAILED")
			return err
		}
		fmt.Println("Complete")
	}

	// Send off a controlled amount of orders and cancels
	if opts.BatchSize > 0 {
		fmt.Print("Sending load transactions...")
		err = plt.sendBatchTradingLoad(marketIDs, opts.UserCount, opts.CommandsPerSecond, opts.RuntimeSeconds, opts.BatchSize, opts.PriceLevels, opts.StartingMidPrice, opts.MoveMid)
		if err != nil {
			fmt.Println("FAILED")
			return err
		}
	} else {
		fmt.Print("Sending load transactions...")
		err = plt.sendTradingLoad(marketIDs, opts.UserCount, opts.CommandsPerSecond, opts.RuntimeSeconds, opts.PriceLevels, opts.StartingMidPrice, opts.MoveMid)
		if err != nil {
			fmt.Println("FAILED")
			return err
		}
	}
	fmt.Println("Complete                      ")

	return nil
}
