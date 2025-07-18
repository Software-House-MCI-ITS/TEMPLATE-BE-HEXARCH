package service

import (
	"context"
	"fmt"
	"log"
	"net/url"
	"strings"
	"time"

	"go-gin-hexagonal/internal/domain/dto"
	"go-gin-hexagonal/internal/domain/entity"
	"go-gin-hexagonal/internal/domain/ports"
	"go-gin-hexagonal/pkg/config"
	"go-gin-hexagonal/pkg/utils"

	"github.com/google/uuid"
)

type AuthService struct {
	userRepo         ports.UserRepository
	refreshTokenRepo ports.RefreshTokenRepository
	tokenManager     ports.TokenManager
	passwordHasher   ports.PasswordHasher
	emailService     ports.EmailService
	aesEncryptor     ports.Encryptor
}

func NewAuthService(
	userRepo ports.UserRepository,
	refreshTokenRepo ports.RefreshTokenRepository,
	tokenManager ports.TokenManager,
	passwordHasher ports.PasswordHasher,
	emailService ports.EmailService,
	aesEncryptor ports.Encryptor,
) ports.AuthService {
	return &AuthService{
		userRepo:         userRepo,
		refreshTokenRepo: refreshTokenRepo,
		tokenManager:     tokenManager,
		passwordHasher:   passwordHasher,
		emailService:     emailService,
		aesEncryptor:     aesEncryptor,
	}
}

func getVerifyEmailURL(token string) string {
	return fmt.Sprintf("%s/verify-email?token=%s", config.GetAppURL(), url.QueryEscape(token))
}

func getResetPasswordURL(token string) string {
	return fmt.Sprintf("%s/reset-password?token=%s", config.GetAppURL(), url.QueryEscape(token))
}

func (s *AuthService) Login(ctx context.Context, req *dto.LoginRequest) (*dto.LoginResponse, error) {
	user, err := s.userRepo.FindByEmail(ctx, req.Email)
	if err != nil {
		return nil, ports.ErrUserNotFound
	}

	if !user.IsActive {
		return nil, ports.ErrUserNotVerified
	}

	if err := s.passwordHasher.Verify(user.Password, req.Password); err != nil {
		return nil, ports.ErrPasswordMismatch
	}

	accessToken, _, err := s.tokenManager.GenerateAccessToken(user)
	if err != nil {
		return nil, err
	}

	refreshToken, refreshTokenExpiry, err := s.tokenManager.GenerateRefreshToken(user.ID)
	if err != nil {
		return nil, err
	}

	refreshTokenEntity := &entity.RefreshToken{
		UserID:    user.ID,
		Token:     refreshToken,
		ExpiresAt: refreshTokenExpiry,
	}
	if err := s.refreshTokenRepo.Save(ctx, refreshTokenEntity); err != nil {
		return nil, err
	}

	return &dto.LoginResponse{
		AccessToken:  accessToken,
		RefreshToken: refreshToken,
	}, nil
}

func (s *AuthService) Register(ctx context.Context, req *dto.RegisterRequest) error {
	if s.userRepo.ExistsByEmail(ctx, req.Email) {
		return ports.ErrUserAlreadyExists
	}

	if s.userRepo.ExistsByUsername(ctx, req.Username) {
		return ports.ErrUserAlreadyExists
	}

	hashedPassword, err := s.passwordHasher.Hash(req.Password)
	if err != nil {
		return err
	}

	user := &entity.User{
		Email:    req.Email,
		Username: req.Username,
		Password: hashedPassword,
		Name:     req.Name,
	}

	plaintext := user.Email + "_" + utils.AddToCurrentTime(5*time.Minute)

	token, err := s.aesEncryptor.Encrypt(plaintext)
	if err != nil {
		return err
	}

	go func(email string, token string) {
		verifyEmailUrl := getVerifyEmailURL(token)
		verifyEmailData := &dto.VerifyEmailData{
			VerificationURL: verifyEmailUrl,
		}
		if err := s.emailService.SendVerifyEmail(email, verifyEmailData); err != nil {
			log.Printf("failed to send verification email: %v", err)
		}
	}(req.Email, token)

	return s.userRepo.Create(ctx, user)
}

func (s *AuthService) RefreshToken(ctx context.Context, req *dto.RefreshTokenRequest) (*dto.RefreshTokenResponse, error) {
	claims, err := s.tokenManager.ValidateRefreshToken(req.RefreshToken)
	if err != nil {
		return nil, ports.ErrTokenInvalid
	}

	if !s.refreshTokenRepo.IsTokenValid(ctx, req.RefreshToken) {
		return nil, ports.ErrTokenInvalid
	}

	user, err := s.userRepo.FindByID(ctx, claims.UserID)
	if err != nil {
		return nil, ports.ErrUserNotFound
	}

	newAccessToken, _, err := s.tokenManager.GenerateAccessToken(user)
	if err != nil {
		return nil, err
	}

	newRefreshToken, refreshTokenExpiry, err := s.tokenManager.GenerateRefreshToken(user.ID)
	if err != nil {
		return nil, err
	}

	s.refreshTokenRepo.RevokeByToken(ctx, req.RefreshToken)

	newRefreshTokenEntity := &entity.RefreshToken{
		UserID:    user.ID,
		Token:     newRefreshToken,
		ExpiresAt: refreshTokenExpiry,
	}
	if err := s.refreshTokenRepo.Save(ctx, newRefreshTokenEntity); err != nil {
		return nil, err
	}

	return &dto.RefreshTokenResponse{
		AccessToken:  newAccessToken,
		RefreshToken: newRefreshToken,
	}, nil
}

func (s *AuthService) Logout(ctx context.Context, userID uuid.UUID) error {
	return s.refreshTokenRepo.RevokeAllByUserID(ctx, userID)
}

func (s *AuthService) SendVerifyEmail(ctx context.Context, email string) error {
	user, err := s.userRepo.FindByEmail(ctx, email)
	if err != nil {
		return ports.ErrUserNotFound
	}

	plaintext := user.Email + "_" + utils.AddToCurrentTime(5*time.Minute)

	token, err := s.aesEncryptor.Encrypt(plaintext)
	if err != nil {
		return err
	}

	go func(email string, token string) {
		verifyEmailUrl := getVerifyEmailURL(token)
		verifyEmailData := &dto.VerifyEmailData{
			VerificationURL: verifyEmailUrl,
		}
		if err := s.emailService.SendVerifyEmail(email, verifyEmailData); err != nil {
			log.Printf("failed to send verification email: %v", err)
		}
	}(email, token)

	return nil
}

func (s *AuthService) VerifyEmail(ctx context.Context, token string) error {
	token, err := s.aesEncryptor.Decrypt(token)
	if err != nil {
		return err
	}

	tokenArr := strings.Split(token, "_")
	if len(tokenArr) != 2 {
		return ports.ErrTokenInvalid
	}

	email := tokenArr[0]
	user, err := s.userRepo.FindByEmail(ctx, email)
	if err != nil {
		return ports.ErrUserNotFound
	}

	expiryStr := tokenArr[1]
	expiryTime, err := utils.ParseTime(expiryStr)
	if err != nil {
		return ports.ErrTokenInvalid
	}
	if time.Now().After(expiryTime) {
		return ports.ErrTokenExpired
	}

	user.IsActive = true

	if err := s.userRepo.Update(ctx, user); err != nil {
		return ports.ErrUpdateUser
	}

	return nil
}

func (s *AuthService) SendResetPassword(ctx context.Context, email string) error {
	user, err := s.userRepo.FindByEmail(ctx, email)
	if err != nil {
		return ports.ErrUserNotFound
	}

	plaintext := user.Email + "_" + utils.AddToCurrentTime(15*time.Minute)

	token, err := s.aesEncryptor.Encrypt(plaintext)
	if err != nil {
		return err
	}

	go func(email string, token string) {
		resetPasswordUrl := getResetPasswordURL(token)
		resetPasswordData := &dto.ResetPasswordData{
			ResetLink: resetPasswordUrl,
		}
		if err := s.emailService.SendRequestResetPassword(email, resetPasswordData); err != nil {
			log.Printf("failed to send reset password email: %v", err)
		}
	}(email, token)

	return nil
}

func (s *AuthService) ResetPassword(ctx context.Context, req *dto.ResetPasswordRequest) error {
	token := req.Token
	token, err := s.aesEncryptor.Decrypt(token)
	if err != nil {
		return err
	}

	tokenArr := strings.Split(token, "_")
	if len(tokenArr) != 2 {
		return ports.ErrTokenInvalid
	}

	user, err := s.userRepo.FindByEmail(ctx, tokenArr[0])
	if err != nil {
		return ports.ErrUserNotFound
	}

	expiryStr := tokenArr[1]
	expiryTime, err := utils.ParseTime(expiryStr)
	if err != nil {
		return ports.ErrTokenInvalid
	}
	if time.Now().After(expiryTime) {
		return ports.ErrTokenExpired
	}

	hashedPassword, err := s.passwordHasher.Hash(req.NewPassword)
	if err != nil {
		return err
	}

	user.Password = hashedPassword
	if err := s.userRepo.Update(ctx, user); err != nil {
		return ports.ErrUpdateUser
	}

	return nil
}
