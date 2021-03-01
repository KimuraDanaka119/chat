package tinode

import java.util.Base64
import java.util.concurrent.ConcurrentHashMap

import scala.collection.JavaConverters._
import scala.collection._
import scala.concurrent.duration._

import io.gatling.core.Predef._
import io.gatling.http.Predef._

class Loadtest extends Simulation {
  val httpProtocol = http
    .baseUrl("http://localhost:6060")
    .wsBaseUrl("ws://localhost:6060")

  // Input file can be set with the "accounts" java option.
  // E.g. JAVA_OPTS="-Daccounts=/tmp/z.csv" gatling.sh -sf . -rsf . -rd "na" -s tinode.Loadtest
  val feeder = csv(System.getProperty("accounts", "users.csv")).random

  // Auth tokens to share between sessions.
  val tokenCache : concurrent.Map[String, String] = new ConcurrentHashMap() asScala

  val loginBasic = exitBlockOnFail {
    exec { session =>
      val uname = session("username").as[String]
      val password = session("password").as[String]
      val secret = new String(java.util.Base64.getEncoder.encode((uname + ":" + password).getBytes()))
      session.set("secret", secret)
    }
    .exec {
      ws("login").sendText(
        """{"login":{"id":"${id}-login","scheme":"basic","secret":"${secret}"}}"""
      )
      .await(5 seconds)(
        ws.checkTextMessage("login-ctrl")
          .matching(jsonPath("$.ctrl").find.exists)
          .check(jsonPath("$.ctrl.params.token").saveAs("token"))
      )
    }
    .exec { session =>
      val uname = session("username").as[String]
      val token = session("token").as[String]
      tokenCache.put(uname, token)
      session
    }
  }

  val loginToken = exitBlockOnFail {
    exec { session =>
      val uname = session("username").as[String]
      var token = session("token").asOption[String]
      if (token == None) {
        token = tokenCache.get(uname)
      }
      session.set("token", token.getOrElse(""))
    }
    .exec {
      ws("login-token").sendText(
        """{"login":{"id":"${id}-login2","scheme":"token","secret":"${token}"}}"""
      )
      .await(5 seconds)(
        ws.checkTextMessage("login-ctrl")
          .matching(jsonPath("$.ctrl").find.exists)
      )
    }
  }

  val subMe = exitBlockOnFail {
    exec {
      ws("sub-me").sendText(
        """{"sub":{"id":"{id}-sub-me","topic":"me","get":{"what":"desc"}}}"""
      )
      .await(5 seconds)(
        ws.checkTextMessage("sub-me-desc")
          .matching(jsonPath("$.ctrl").find.exists)
          .check(jsonPath("$.ctrl.code").ofType[Int].in(200 to 299))
      )
    }
  }

  val publish = exitBlockOnFail {
    exec {
      repeat(3, "i") {
        exec {
          ws("pub-topic").sendText(
            """{"pub":{"id":"${id}-pub-${sub}-${i}","topic":"${sub}","content":"This is a Tsung test ${i}"}}"""
          )
          .await(15 seconds)(
            ws.checkTextMessage("pub-topic-ctrl")
              .matching(jsonPath("$.ctrl").find.exists)
              .check(jsonPath("$.ctrl.code").ofType[Int].in(200 to 299))
          )
        }
        .pause(0, 3)
      }
    }
  }

  val getSubs = exitBlockOnFail {
    exec {
      ws("get-subs").sendText(
        """{"get":{"id":"${id}-get-subs","topic":"me","what":"sub"}}"""
      )
      .await(5 seconds)(
        ws.checkTextMessage("save-subs")
          .matching(jsonPath("$.meta.sub").find.exists)
          .check(jsonPath("$.meta.sub[*].topic").findAll.saveAs("subs"))
      )
    }
  }

  val scn = scenario("WebSocket")
    .exec(ws("Connect WS").connect("/v0/channels?apikey=AQEAAAABAAD_rAp4DJh05a1HAwFT3A6K"))
    .exec(session => session.set("id", "tn-" + session.userId))
    .pause(1)
    .exec {
      ws("hi").sendText(
        """{"hi":{"id":"afabb3","ver":"0.16","ua":"Gatling-Loadtest/1.0; gatling/1.7.0"}}"""
      )
      .await(5 seconds)(
        ws.checkTextMessage("hi")
          .matching(jsonPath("$.ctrl").find.exists)
      )
    }
    .pause(1)
    .feed(feeder)
    .doIfOrElse({session =>
      val uname = session("username").as[String]
      var token = session("token").asOption[String]
      if (token == None) {
        token = tokenCache.get(uname)
      }
      token == None
    }) { loginBasic } { loginToken }
    .exitHereIfFailed
    .exec(subMe)
    .exitHereIfFailed
    .exec(getSubs)
    .exitHereIfFailed
    .doIf({session =>
      session.attributes.contains("subs")
    }) {
      exec { session =>
        // Shuffle subscriptions.
        val subs = session("subs").as[Vector[String]]
        val shuffled = scala.util.Random.shuffle(subs.toList)
        session.set("subs", shuffled)
      }
      .foreach("${subs}", "sub") {
        exec {
          ws("sub-topic").sendText(
            """{"sub":{"id":"${id}-sub-${sub}","topic":"${sub}","get":{"what":"desc sub data del"}}}"""
          )
          .await(15 seconds)(
            ws.checkTextMessage("sub-topic-ctrl")
              .matching(jsonPath("$.ctrl").find.exists)
              .check(jsonPath("$.ctrl.code").ofType[Int].in(200 to 299))
          )
        }
        .exitHereIfFailed
        .pause(0, 2)
        .doIfOrElse({session =>
          val topic = session("sub").as[String]
          !topic.startsWith("chn")
        }) { publish } { pause(5) }
        .exec {
          ws("leave-topic").sendText(
            """{"leave":{"id":"${id}-leave-${sub}","topic":"${sub}"}}"""
          )
          .await(5 seconds)(
            ws.checkTextMessage("sub-topic-ctrl")
              .matching(jsonPath("$.ctrl").find.exists)
          )
        }
        .pause(0, 3)
      }
    }
    .exec(ws("close-ws").close)

  val numUsers = Integer.getInteger("num_users", 10000)
  val rampPeriod = java.lang.Long.getLong("ramp", 300L)
  setUp(scn.inject(rampUsers(numUsers) during (rampPeriod.seconds))).protocols(httpProtocol)
}
